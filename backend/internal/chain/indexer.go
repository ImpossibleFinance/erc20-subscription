package chain

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// Decoded events.

type ChargedEvent struct {
	User   common.Address
	Amount *big.Int
	TxHash common.Hash
	Block  uint64
}

type CancelledEvent struct {
	User   common.Address
	TxHash common.Hash
	Block  uint64
}

// HeadBlock returns the current chain head minus `reorgLag` blocks. Indexer
// should never query past this height.
func (c *Client) HeadBlock(ctx context.Context, reorgLag uint64) (uint64, error) {
	h, err := c.rpc.HeaderByNumber(ctx, nil)
	if err != nil {
		return 0, err
	}
	n := h.Number.Uint64()
	if n < reorgLag {
		return 0, nil
	}
	return n - reorgLag, nil
}

// FetchLogs pulls all Subscriptions logs in [from, to] inclusive.
func (c *Client) FetchLogs(ctx context.Context, from, to uint64) ([]types.Log, error) {
	q := ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(from),
		ToBlock:   new(big.Int).SetUint64(to),
		Addresses: []common.Address{c.contract},
	}
	return c.rpc.FilterLogs(ctx, q)
}

// DecodeLog inspects a single log and returns a typed event, or (nil, nil) if
// the topic doesn't match any known event.
func DecodeLog(l types.Log) (interface{}, error) {
	if len(l.Topics) == 0 {
		return nil, nil
	}
	switch l.Topics[0] {
	case SubscriptionsABI.Events["Charged"].ID:
		return decodeCharged(l)
	case SubscriptionsABI.Events["Cancelled"].ID:
		return decodeCancelled(l)
	}
	return nil, nil
}

func decodeCharged(l types.Log) (*ChargedEvent, error) {
	if len(l.Topics) != 2 {
		return nil, fmt.Errorf("Charged: want 2 topics, got %d", len(l.Topics))
	}
	out, err := SubscriptionsABI.Events["Charged"].Inputs.NonIndexed().Unpack(l.Data)
	if err != nil {
		return nil, fmt.Errorf("Charged: %w", err)
	}
	return &ChargedEvent{
		User:   common.BytesToAddress(l.Topics[1].Bytes()),
		Amount: out[0].(*big.Int),
		TxHash: l.TxHash,
		Block:  l.BlockNumber,
	}, nil
}

func decodeCancelled(l types.Log) (*CancelledEvent, error) {
	if len(l.Topics) != 2 {
		return nil, fmt.Errorf("Cancelled: want 2 topics, got %d", len(l.Topics))
	}
	return &CancelledEvent{
		User:   common.BytesToAddress(l.Topics[1].Bytes()),
		TxHash: l.TxHash,
		Block:  l.BlockNumber,
	}, nil
}
