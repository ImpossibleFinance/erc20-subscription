// Package chain wraps the on-chain Subscriptions contract with just enough
// surface for the scheduler, indexer, and admin API: submit pull(), decode
// Subscribed/Cancelled/Charged event logs.
package chain

import (
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
)

const subscriptionsABIJSON = `[
  {"type":"function","name":"pull","stateMutability":"nonpayable","inputs":[
    {"name":"user","type":"address"},
    {"name":"amount","type":"uint256"}
  ],"outputs":[]},
  {"type":"function","name":"cancel","stateMutability":"nonpayable","inputs":[],"outputs":[]},
  {"type":"function","name":"setPaused","stateMutability":"nonpayable","inputs":[{"name":"p","type":"bool"}],"outputs":[]},
  {"type":"event","name":"Cancelled","inputs":[
    {"name":"user","type":"address","indexed":true}
  ],"anonymous":false},
  {"type":"event","name":"Charged","inputs":[
    {"name":"user","type":"address","indexed":true},
    {"name":"amount","type":"uint256","indexed":false}
  ],"anonymous":false}
]`

// ERC-20 — just the view methods we need (allowance lookup).
const erc20ABIJSON = `[
  {"type":"function","name":"allowance","stateMutability":"view","inputs":[
    {"name":"owner","type":"address"},
    {"name":"spender","type":"address"}
  ],"outputs":[{"name":"","type":"uint256"}]}
]`

// Parsed ABIs, used everywhere we need to encode calls or decode events.
var (
	SubscriptionsABI abi.ABI
	ERC20ABI         abi.ABI
)

func init() {
	parsed, err := abi.JSON(strings.NewReader(subscriptionsABIJSON))
	if err != nil {
		panic("chain: failed to parse Subscriptions ABI: " + err.Error())
	}
	SubscriptionsABI = parsed

	parsed20, err := abi.JSON(strings.NewReader(erc20ABIJSON))
	if err != nil {
		panic("chain: failed to parse ERC20 ABI: " + err.Error())
	}
	ERC20ABI = parsed20
}
