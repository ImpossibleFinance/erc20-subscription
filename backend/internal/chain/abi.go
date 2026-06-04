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
  {"type":"function","name":"treasury","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"address"}]},
  {"type":"function","name":"token","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"address"}]},
  {"type":"event","name":"Cancelled","inputs":[
    {"name":"user","type":"address","indexed":true}
  ],"anonymous":false},
  {"type":"event","name":"Charged","inputs":[
    {"name":"user","type":"address","indexed":true},
    {"name":"amount","type":"uint256","indexed":false}
  ],"anonymous":false}
]`

// ERC-20 — the view methods we need (allowance lookup) plus the state-
// changing methods so the checkout-session verifier can decode calldata for a
// user's `approve` and `transfer` txs, and the Transfer event log so we can
// cross-check what the calldata says against what the receipt logged.
const erc20ABIJSON = `[
  {"type":"function","name":"allowance","stateMutability":"view","inputs":[
    {"name":"owner","type":"address"},
    {"name":"spender","type":"address"}
  ],"outputs":[{"name":"","type":"uint256"}]},
  {"type":"function","name":"transfer","stateMutability":"nonpayable","inputs":[
    {"name":"to","type":"address"},
    {"name":"value","type":"uint256"}
  ],"outputs":[{"name":"","type":"bool"}]},
  {"type":"function","name":"approve","stateMutability":"nonpayable","inputs":[
    {"name":"spender","type":"address"},
    {"name":"value","type":"uint256"}
  ],"outputs":[{"name":"","type":"bool"}]},
  {"type":"function","name":"symbol","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"string"}]},
  {"type":"function","name":"decimals","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"uint8"}]},
  {"type":"event","name":"Transfer","inputs":[
    {"name":"from","type":"address","indexed":true},
    {"name":"to","type":"address","indexed":true},
    {"name":"value","type":"uint256","indexed":false}
  ],"anonymous":false}
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
