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

// Pull calls Subscriptions.pull(user, amount). Returns the tx hash; the caller
// should wait for receipt to confirm. Reverts are surfaced as errors.
func (c *Client) Pull(ctx context.Context, user common.Address, amount *big.Int) (common.Hash, error) {
	return c.sendCall(ctx, "pull", user, amount)
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

func (c *Client) sendCall(ctx context.Context, method string, args ...interface{}) (common.Hash, error) {
	data, err := SubscriptionsABI.Pack(method, args...)
	if err != nil {
		return common.Hash{}, fmt.Errorf("pack %s: %w", method, err)
	}

	nonce, err := c.rpc.PendingNonceAt(ctx, c.opAddr)
	if err != nil {
		return common.Hash{}, fmt.Errorf("pending nonce: %w", err)
	}
	tip, err := c.rpc.SuggestGasTipCap(ctx)
	if err != nil {
		return common.Hash{}, fmt.Errorf("gas tip: %w", err)
	}
	head, err := c.rpc.HeaderByNumber(ctx, nil)
	if err != nil {
		return common.Hash{}, fmt.Errorf("head: %w", err)
	}
	maxFee := new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(2)), tip)

	msg := ethereum.CallMsg{From: c.opAddr, To: &c.contract, Data: data}
	gas, err := c.rpc.EstimateGas(ctx, msg)
	if err != nil {
		return common.Hash{}, fmt.Errorf("estimate %s: %w", method, err)
	}

	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   c.chainID,
		Nonce:     nonce,
		GasTipCap: tip,
		GasFeeCap: maxFee,
		Gas:       gas,
		To:        &c.contract,
		Data:      data,
	})
	signed, err := types.SignTx(tx, types.LatestSignerForChainID(c.chainID), c.opKey)
	if err != nil {
		return common.Hash{}, fmt.Errorf("sign: %w", err)
	}
	if err := c.rpc.SendTransaction(ctx, signed); err != nil {
		return common.Hash{}, fmt.Errorf("send: %w", err)
	}
	return signed.Hash(), nil
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
