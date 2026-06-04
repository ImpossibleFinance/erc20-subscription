package chain

import (
	"encoding/hex"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

func TestDecodeTransferCall(t *testing.T) {
	to := common.HexToAddress("0xd5992bb8F84254562d67c9E389b10fb9D4e6FBbc")
	value := big.NewInt(49_000_000) // 49 USDC in atomic units
	calldata := mustPackTransfer(t, to, value)

	gotTo, gotValue, ok := DecodeTransferCall(calldata)
	if !ok {
		t.Fatalf("decode failed")
	}
	if gotTo != to {
		t.Fatalf("recipient: got %s, want %s", gotTo.Hex(), to.Hex())
	}
	if gotValue.Cmp(value) != 0 {
		t.Fatalf("value: got %s, want %s", gotValue, value)
	}
}

func TestDecodeTransferCallRejectsWrongSelector(t *testing.T) {
	// Approve selector — different from transfer.
	to := common.HexToAddress("0xd5992bb8F84254562d67c9E389b10fb9D4e6FBbc")
	approveData := mustPackApprove(t, to, big.NewInt(1))
	if _, _, ok := DecodeTransferCall(approveData); ok {
		t.Fatalf("approve calldata must not decode as transfer")
	}
}

func TestDecodeTransferCallRejectsShortInput(t *testing.T) {
	if _, _, ok := DecodeTransferCall([]byte{0xa9}); ok {
		t.Fatalf("short input must not decode")
	}
	if _, _, ok := DecodeTransferCall(nil); ok {
		t.Fatalf("nil input must not decode")
	}
}

func TestDecodeApproveCall(t *testing.T) {
	spender := common.HexToAddress("0x89EDCe46cF42666e27a2fA099Ee5Df25B62736Eb")
	value := big.NewInt(539_000_000) // 11 months of $49
	calldata := mustPackApprove(t, spender, value)

	gotSpender, gotValue, ok := DecodeApproveCall(calldata)
	if !ok {
		t.Fatalf("decode failed")
	}
	if gotSpender != spender {
		t.Fatalf("spender: got %s, want %s", gotSpender.Hex(), spender.Hex())
	}
	if gotValue.Cmp(value) != 0 {
		t.Fatalf("value: got %s, want %s", gotValue, value)
	}
}

func TestDecodeTransferLog(t *testing.T) {
	token := common.HexToAddress("0x036CbD53842c5426634e7929541eC2318f3dCF7e")
	from := common.HexToAddress("0xbeefbeefbeefbeefbeefbeefbeefbeefbeefbeef")
	to := common.HexToAddress("0xd5992bb8F84254562d67c9E389b10fb9D4e6FBbc")
	value := big.NewInt(49_000_000)

	transferTopic := crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))
	receipt := &types.Receipt{
		Logs: []*types.Log{{
			Address: token,
			Topics: []common.Hash{
				transferTopic,
				common.BytesToHash(from.Bytes()),
				common.BytesToHash(to.Bytes()),
			},
			Data: common.LeftPadBytes(value.Bytes(), 32),
		}},
	}
	gotFrom, gotTo, ok := DecodeTransferLog(receipt, token, value)
	if !ok {
		t.Fatalf("decode log failed")
	}
	if gotFrom != from || gotTo != to {
		t.Fatalf("log addresses mismatch: from=%s to=%s want from=%s to=%s",
			gotFrom.Hex(), gotTo.Hex(), from.Hex(), to.Hex())
	}
}

func TestDecodeTransferLogIgnoresOtherTokens(t *testing.T) {
	token := common.HexToAddress("0x036CbD53842c5426634e7929541eC2318f3dCF7e")
	other := common.HexToAddress("0x1111111111111111111111111111111111111111")
	value := big.NewInt(49_000_000)
	transferTopic := crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))
	receipt := &types.Receipt{Logs: []*types.Log{{
		Address: other, // different token contract — should be skipped
		Topics:  []common.Hash{transferTopic, {}, {}},
		Data:    common.LeftPadBytes(value.Bytes(), 32),
	}}}
	if _, _, ok := DecodeTransferLog(receipt, token, value); ok {
		t.Fatalf("must not match a log from a different contract")
	}
}

func TestDecodeTransferLogIgnoresWrongValue(t *testing.T) {
	token := common.HexToAddress("0x036CbD53842c5426634e7929541eC2318f3dCF7e")
	transferTopic := crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))
	receipt := &types.Receipt{Logs: []*types.Log{{
		Address: token,
		Topics:  []common.Hash{transferTopic, {}, {}},
		Data:    common.LeftPadBytes(big.NewInt(1).Bytes(), 32),
	}}}
	if _, _, ok := DecodeTransferLog(receipt, token, big.NewInt(2)); ok {
		t.Fatalf("must not match a log with a different value")
	}
}

func mustPackTransfer(t *testing.T, to common.Address, value *big.Int) []byte {
	t.Helper()
	method := ERC20ABI.Methods["transfer"]
	args, err := method.Inputs.Pack(to, value)
	if err != nil {
		t.Fatal(err)
	}
	return append(method.ID, args...)
}

func mustPackApprove(t *testing.T, spender common.Address, value *big.Int) []byte {
	t.Helper()
	method := ERC20ABI.Methods["approve"]
	args, err := method.Inputs.Pack(spender, value)
	if err != nil {
		t.Fatal(err)
	}
	return append(method.ID, args...)
}

// confirms selectors stay stable across go-ethereum versions; if they drift,
// downstream verification silently breaks.
func TestSelectorsAreCanonical(t *testing.T) {
	if strings.ToLower(hex.EncodeToString(transferSelector)) != "a9059cbb" {
		t.Fatalf("transfer selector: got %x, want a9059cbb", transferSelector)
	}
	if strings.ToLower(hex.EncodeToString(approveSelector)) != "095ea7b3" {
		t.Fatalf("approve selector: got %x, want 095ea7b3", approveSelector)
	}
}
