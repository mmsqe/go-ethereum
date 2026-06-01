// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package runtime

import (
	"bytes"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/params"
)

// runcodeWrapper builds an outer program that CODECOPYs innerCode into memory at
// 0x40, RUNCODEs it (no args, all gas), and returns the 32 bytes it left in
// mem[0:32]. innerOff is the prologue length; the guard keeps them in sync.
func runcodeWrapper(innerCode []byte) []byte {
	const innerOff = 0x1b
	ll := byte(len(innerCode))
	prologue := []byte{
		0x60, ll, 0x60, innerOff, 0x60, 0x40, 0x39, // CODECOPY inner -> mem[0x40]
		0x60, 0x20, 0x60, 0x00, // retLength, retOffset
		0x60, 0x00, 0x60, 0x00, // argsLength, argsOffset
		0x60, ll, 0x60, 0x40, // codeLength, codeOffset
		0x5a,                         // GAS (forward all)
		byte(vm.RUNCODE),             // 0xf6
		0x50,                         // POP success flag
		0x60, 0x20, 0x60, 0x00, 0xf3, // RETURN mem[0:32]
	}
	if len(prologue) != innerOff {
		panic("runcodeWrapper: prologue length drifted from innerOff")
	}
	return append(prologue, innerCode...)
}

// runRuncode executes code with the RUNCODE opcode (EIP-7990) enabled and
// returns its output, failing the test on any execution error.
func runRuncode(t *testing.T, code []byte) []byte {
	t.Helper()
	cfg := &Config{}
	cfg.EVMConfig.ExtraEips = []int{7990}
	ret, _, err := Execute(code, nil, cfg)
	if err != nil {
		t.Fatalf("execution failed: %v", err)
	}
	return ret
}

// TestRuncodeCodecopyAddressesInMemoryBlob is the load-bearing test for the
// pydefi EIP-7990 migration: CODECOPY run *inside* a RUNCODE frame must address
// the in-memory code blob (like deployed code), so pydefi's embed_and_load /
// data-section codegen works unchanged. innerCode CODECOPYs a byte embedded in
// its own code (0x42 at offset 0x0c) into mem and returns it.
func TestRuncodeCodecopyAddressesInMemoryBlob(t *testing.T) {
	innerCode := []byte{
		0x60, 0x01, 0x60, 0x0c, 0x60, 0x1f, 0x39, // CODECOPY code[0x0c] -> mem[0x1f]
		0x60, 0x20, 0x60, 0x00, 0xf3, // RETURN mem[0:32]
		0x42, // data byte, reachable only via CODECOPY
	}
	ret := runRuncode(t, runcodeWrapper(innerCode))
	if len(ret) != 32 {
		t.Fatalf("expected 32-byte return, got %d bytes", len(ret))
	}
	if ret[31] != 0x42 {
		t.Fatalf("CODECOPY inside RUNCODE did not read the in-memory blob: got 0x%02x, want 0x42", ret[31])
	}
}

// TestRuncodePreservesContext proves RUNCODE runs in the caller's context:
// ADDRESS inside the inner program equals the outer contract's address.
func TestRuncodePreservesContext(t *testing.T) {
	innerCode := []byte{0x30, 0x60, 0x00, 0x52, 0x60, 0x20, 0x60, 0x00, 0xf3} // ADDRESS; MSTORE 0; RETURN mem[0:32]
	ret := runRuncode(t, runcodeWrapper(innerCode))
	// runtime.Execute deploys the code at address("contract").
	want := common.BytesToAddress([]byte("contract"))
	if got := common.BytesToAddress(ret[12:32]); got != want {
		t.Fatalf("ADDRESS inside RUNCODE = %s, want caller context %s", got, want)
	}
}

// TestRuncodeRealPydefiProgram runs a real pydefi-compiled program through
// RUNCODE: the verbatim output of embed_and_load(bytes(range(1,33))) + return_,
// which CODECOPYs its embedded 32-byte data section and returns it. Proving it
// returns that data confirms pydefi's data-section/CODECOPY codegen works
// natively in-context — the cross-repo answer to the migration's open question.
func TestRuncodeRealPydefiProgram(t *testing.T) {
	pydefiProgram := common.FromHex("602061000b5f3960205ff30102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	wantData := common.FromHex("0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	ret := runRuncode(t, runcodeWrapper(pydefiProgram))
	if !bytes.Equal(ret, wantData) {
		t.Fatalf("pydefi program under RUNCODE returned %x, want %x", ret, wantData)
	}
}

// TestRuncodeGasAccounting asserts RUNCODE charges its EIP-7990 base cost in
// isolation: 7 PUSH0 args (2 gas each) + an empty inner program (no memory
// expansion, no forwarded gas) + STOP = 7*GasQuickStep + RuncodeGas = 114.
func TestRuncodeGasAccounting(t *testing.T) {
	code := []byte{0x5f, 0x5f, 0x5f, 0x5f, 0x5f, 0x5f, 0x5f, byte(vm.RUNCODE), 0x00} // 7x PUSH0; RUNCODE; STOP

	var gasUsed uint64
	cfg := &Config{GasLimit: 1_000_000}
	cfg.EVMConfig.ExtraEips = []int{7990}
	cfg.EVMConfig.Tracer = &tracing.Hooks{
		OnTxEnd: func(receipt *types.Receipt, err error) { gasUsed = receipt.GasUsed },
	}
	if _, _, err := Execute(code, nil, cfg); err != nil {
		t.Fatalf("execution failed: %v", err)
	}

	want := 7*vm.GasQuickStep + params.RuncodeGas
	if gasUsed != want {
		t.Fatalf("RUNCODE gas accounting: used %d, want %d (7 PUSH0 + RuncodeGas base)", gasUsed, want)
	}
}

// TestRuncodeGatedByEIP confirms 0xf6 is an undefined opcode unless EIP-7990 is
// enabled, so it stays inert on chains that have not activated it.
func TestRuncodeGatedByEIP(t *testing.T) {
	code := runcodeWrapper([]byte{0x60, 0x20, 0x60, 0x00, 0xf3}) // PUSH1 0x20; PUSH1 0x00; RETURN

	// No ExtraEips → RUNCODE (0xf6) must be rejected as an invalid opcode.
	_, _, err := Execute(code, nil, &Config{})
	if err == nil {
		t.Fatal("expected RUNCODE to be an invalid opcode when EIP-7990 is disabled")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("invalid opcode")) {
		t.Fatalf("expected invalid-opcode error, got: %v", err)
	}
}
