package chain

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// Client wraps an RPC connection and an operator signing key.
type Client struct {
	rpc      *ethclient.Client
	contract common.Address
	chainID  *big.Int
	opKey    *ecdsa.PrivateKey
	opAddr   common.Address
}

func New(ctx context.Context, rpcURL, contractAddr string, chainID int64, opKeyHex string) (*Client, error) {
	rpc, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return nil, fmt.Errorf("dial rpc: %w", err)
	}
	key, err := crypto.HexToECDSA(strings.TrimPrefix(opKeyHex, "0x"))
	if err != nil {
		return nil, fmt.Errorf("operator key: %w", err)
	}
	return &Client{
		rpc:      rpc,
		contract: common.HexToAddress(contractAddr),
		chainID:  big.NewInt(chainID),
		opKey:    key,
		opAddr:   crypto.PubkeyToAddress(key.PublicKey),
	}, nil
}

func (c *Client) OperatorAddress() common.Address { return c.opAddr }
func (c *Client) Contract() common.Address        { return c.contract }
func (c *Client) RPC() *ethclient.Client          { return c.rpc }

// SubmitPull wraps SubmitAndWait for the Subscriptions.pull(user, amount)
// call. Handles bump-and-replace on stuck txs (only when fees are actually
// underbid). Returns the receipt or ErrTxStuck.
//
// `onSubmit` is invoked with each broadcast tx hash so the scheduler can
// persist InFlightTx for crash recovery.
func (c *Client) SubmitPull(ctx context.Context, user common.Address, amount *big.Int, onSubmit func(common.Hash) error) (*types.Receipt, error) {
	return c.SubmitAndWait(ctx, "pull", []interface{}{user, amount}, onSubmit)
}

// Balance returns the native ETH balance of `addr` in wei. Used by the
// scheduler to detect when the operator's gas budget runs low.
func (c *Client) Balance(ctx context.Context, addr common.Address) (*big.Int, error) {
	return c.rpc.BalanceAt(ctx, addr, nil)
}

// EstimatedGasPricePerUnit returns the current (base fee + suggested tip) in
// wei. Multiply by gas-units to get the wei cost of a tx at current prices.
func (c *Client) EstimatedGasPricePerUnit(ctx context.Context) (*big.Int, error) {
	head, err := c.rpc.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, err
	}
	tip, err := c.rpc.SuggestGasTipCap(ctx)
	if err != nil {
		return nil, err
	}
	return new(big.Int).Add(head.BaseFee, tip), nil
}

// Allowance returns the user's current ERC-20 allowance to `spender` on
// `token`. Used by the scheduler to detect when a user is about to run out of
// approved budget so we can warn them before pulls start failing.
func (c *Client) Allowance(ctx context.Context, token, owner, spender common.Address) (*big.Int, error) {
	data, err := ERC20ABI.Pack("allowance", owner, spender)
	if err != nil {
		return nil, fmt.Errorf("pack allowance: %w", err)
	}
	res, err := c.rpc.CallContract(ctx, ethereum.CallMsg{To: &token, Data: data}, nil)
	if err != nil {
		return nil, fmt.Errorf("call allowance: %w", err)
	}
	out, err := ERC20ABI.Unpack("allowance", res)
	if err != nil {
		return nil, fmt.Errorf("unpack allowance: %w", err)
	}
	return out[0].(*big.Int), nil
}

// WaitReceipt polls until the tx is mined and returns its receipt. Returns
// (receipt, error). If the tx reverted, error is non-nil but receipt is also
// returned so the caller can read the revert reason.
func (c *Client) WaitReceipt(ctx context.Context, hash common.Hash) (*types.Receipt, error) {
	for {
		r, err := c.rpc.TransactionReceipt(ctx, hash)
		if err == nil && r != nil {
			if r.Status != types.ReceiptStatusSuccessful {
				return r, fmt.Errorf("tx reverted: %s", hash.Hex())
			}
			return r, nil
		}
		if err != nil && err != ethereum.NotFound {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
