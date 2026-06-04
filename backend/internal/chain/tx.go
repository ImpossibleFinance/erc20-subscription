package chain

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// FetchedTx bundles everything the checkout-session verifier needs about a
// single tx hash: the tx itself, its receipt, the recovered sender, and how
// many blocks have been mined on top of it.
type FetchedTx struct {
	Tx            *types.Transaction
	Receipt       *types.Receipt
	From          common.Address
	Confirmations uint64
}

// ErrTxNotMined indicates the tx hash is unknown to the node OR no receipt is
// available yet (still pending). Maps to the API's `transfer_not_mined` /
// `approval_not_mined` error code; callers should advise the page to retry.
var ErrTxNotMined = errors.New("tx not yet mined")

// Fetch loads a tx + receipt by hash and computes its confirmation depth.
// Returns ErrTxNotMined when the tx is unknown or the receipt isn't available
// (in either order — both manifest the same to an end user). Other RPC
// errors propagate as-is.
func (c *Client) Fetch(ctx context.Context, hash common.Hash) (*FetchedTx, error) {
	tx, isPending, err := c.rpc.TransactionByHash(ctx, hash)
	if err == ethereum.NotFound {
		return nil, ErrTxNotMined
	}
	if err != nil {
		return nil, fmt.Errorf("get tx: %w", err)
	}
	if isPending {
		return nil, ErrTxNotMined
	}

	receipt, err := c.rpc.TransactionReceipt(ctx, hash)
	if err == ethereum.NotFound {
		return nil, ErrTxNotMined
	}
	if err != nil {
		return nil, fmt.Errorf("get receipt: %w", err)
	}

	signer := types.LatestSignerForChainID(c.chainID)
	from, err := types.Sender(signer, tx)
	if err != nil {
		return nil, fmt.Errorf("recover sender: %w", err)
	}

	head, err := c.rpc.BlockNumber(ctx)
	if err != nil {
		return nil, fmt.Errorf("head block: %w", err)
	}
	var confs uint64
	if head >= receipt.BlockNumber.Uint64() {
		confs = head - receipt.BlockNumber.Uint64() + 1
	}

	return &FetchedTx{
		Tx:            tx,
		Receipt:       receipt,
		From:          from,
		Confirmations: confs,
	}, nil
}

// transferSelector / approveSelector are the 4-byte function selectors for the
// ERC-20 calls we decode in the checkout verifier. Computed at init from the
// parsed ABI so a typo here can't drift.
var (
	transferSelector []byte
	approveSelector  []byte
)

func init() {
	transferSelector = ERC20ABI.Methods["transfer"].ID
	approveSelector = ERC20ABI.Methods["approve"].ID
}

// DecodeTransferCall decodes ERC-20 `transfer(to, value)` calldata. Returns
// (recipient, amount, ok). `ok` is false if the input doesn't start with the
// transfer selector, has the wrong length, or fails ABI decoding.
func DecodeTransferCall(input []byte) (common.Address, *big.Int, bool) {
	if len(input) < 4 || !bytesEqual(input[:4], transferSelector) {
		return common.Address{}, nil, false
	}
	args, err := ERC20ABI.Methods["transfer"].Inputs.Unpack(input[4:])
	if err != nil || len(args) != 2 {
		return common.Address{}, nil, false
	}
	to, ok := args[0].(common.Address)
	if !ok {
		return common.Address{}, nil, false
	}
	value, ok := args[1].(*big.Int)
	if !ok || value == nil {
		return common.Address{}, nil, false
	}
	return to, value, true
}

// DecodeApproveCall decodes ERC-20 `approve(spender, value)` calldata.
func DecodeApproveCall(input []byte) (common.Address, *big.Int, bool) {
	if len(input) < 4 || !bytesEqual(input[:4], approveSelector) {
		return common.Address{}, nil, false
	}
	args, err := ERC20ABI.Methods["approve"].Inputs.Unpack(input[4:])
	if err != nil || len(args) != 2 {
		return common.Address{}, nil, false
	}
	spender, ok := args[0].(common.Address)
	if !ok {
		return common.Address{}, nil, false
	}
	value, ok := args[1].(*big.Int)
	if !ok || value == nil {
		return common.Address{}, nil, false
	}
	return spender, value, true
}

// DecodeTransferLog scans the receipt's logs for the canonical ERC-20
// Transfer event emitted by `token` whose value matches `expected`. Used to
// cross-check what the calldata says against what the chain actually logged
// — defends against the (rare) exotic-token case where calldata and event
// disagree.
//
// Returns (from, to, ok). ok is true if a matching log was found.
func DecodeTransferLog(receipt *types.Receipt, token common.Address, expectedValue *big.Int) (from, to common.Address, ok bool) {
	if receipt == nil || expectedValue == nil {
		return common.Address{}, common.Address{}, false
	}
	transferTopic := ERC20ABI.Events["Transfer"].ID
	for _, log := range receipt.Logs {
		if log.Address != token {
			continue
		}
		if len(log.Topics) != 3 {
			continue
		}
		if log.Topics[0] != transferTopic {
			continue
		}
		// data is the non-indexed value; 32 bytes.
		if len(log.Data) != 32 {
			continue
		}
		value := new(big.Int).SetBytes(log.Data)
		if value.Cmp(expectedValue) != 0 {
			continue
		}
		from = common.BytesToAddress(log.Topics[1].Bytes())
		to = common.BytesToAddress(log.Topics[2].Bytes())
		return from, to, true
	}
	return common.Address{}, common.Address{}, false
}

// ChainID returns the chain id this client is bound to. Handlers use it to
// pin the `Chain:` line in the EIP-191 challenge.
func (c *Client) ChainID() int64 { return c.chainID.Int64() }

// Treasury reads the Subscriptions contract's `treasury()` view. The
// checkout-session API uses this as the authoritative destination for the
// user's first-month direct transfer.
func (c *Client) Treasury(ctx context.Context) (common.Address, error) {
	return c.callAddressView(ctx, "treasury")
}

// TokenInfo fetches `symbol()` and `decimals()` from the configured ERC-20.
// The checkout-session GET endpoint surfaces these so the page can render
// human-readable amounts without hardcoding token assumptions.
func (c *Client) TokenInfo(ctx context.Context, token common.Address) (symbol string, decimals uint8, err error) {
	symData, err := ERC20ABI.Pack("symbol")
	if err != nil {
		return "", 0, fmt.Errorf("pack symbol: %w", err)
	}
	symRes, err := c.rpc.CallContract(ctx, ethereum.CallMsg{To: &token, Data: symData}, nil)
	if err != nil {
		return "", 0, fmt.Errorf("call symbol: %w", err)
	}
	symOut, err := ERC20ABI.Unpack("symbol", symRes)
	if err != nil || len(symOut) == 0 {
		return "", 0, fmt.Errorf("unpack symbol: %w", err)
	}
	symbol, _ = symOut[0].(string)

	decData, err := ERC20ABI.Pack("decimals")
	if err != nil {
		return "", 0, fmt.Errorf("pack decimals: %w", err)
	}
	decRes, err := c.rpc.CallContract(ctx, ethereum.CallMsg{To: &token, Data: decData}, nil)
	if err != nil {
		return "", 0, fmt.Errorf("call decimals: %w", err)
	}
	decOut, err := ERC20ABI.Unpack("decimals", decRes)
	if err != nil || len(decOut) == 0 {
		return "", 0, fmt.Errorf("unpack decimals: %w", err)
	}
	decimals, _ = decOut[0].(uint8)
	return symbol, decimals, nil
}

func (c *Client) callAddressView(ctx context.Context, method string) (common.Address, error) {
	data, err := SubscriptionsABI.Pack(method)
	if err != nil {
		return common.Address{}, fmt.Errorf("pack %s: %w", method, err)
	}
	res, err := c.rpc.CallContract(ctx, ethereum.CallMsg{To: &c.contract, Data: data}, nil)
	if err != nil {
		return common.Address{}, fmt.Errorf("call %s: %w", method, err)
	}
	out, err := SubscriptionsABI.Unpack(method, res)
	if err != nil || len(out) == 0 {
		return common.Address{}, fmt.Errorf("unpack %s: %w", method, err)
	}
	addr, ok := out[0].(common.Address)
	if !ok {
		return common.Address{}, fmt.Errorf("%s: not an address", method)
	}
	return addr, nil
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
