// Package config loads runtime configuration from environment variables. All
// values are required unless documented otherwise; missing values fail fast at
// startup rather than blowing up mid-flight.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	// Chain.
	RPCURL       string
	ChainID      int64
	ContractAddr string // 0x… Subscriptions contract
	TokenAddr    string // 0x… ERC-20 (USDC) — informational; the contract is the source of truth.

	// Operator key — hex-encoded private key, no 0x prefix. The hot key that
	// signs charge/deactivate calls. Should have ETH for gas and no other
	// privileges anywhere.
	OperatorKeyHex string

	// Storage.
	RedisURL string

	// Scheduler cadence — how often we scan for due subscriptions.
	TickInterval time.Duration

	// Dunning retry schedule in order. After the last entry, we give up and
	// call deactivate(). Defaults to 24h, 72h, 168h.
	DunningBackoffs []time.Duration

	// Webhook config.
	WebhookURL    string // optional. If unset, webhooks are logged only.
	WebhookSecret string // HMAC-SHA256 key for X-Signature header.

	// AllowanceLowMonths: after a successful pull, if the user's remaining
	// USDC allowance covers fewer than this many additional periods, emit a
	// `subscription.allowance_low` webhook so the integrator can prompt them
	// to re-approve before pulls start failing. 0 disables the check.
	AllowanceLowMonths int

	// OperatorGasBufferMonths: warn (via `operator.gas_low` webhook) when
	// the operator EOA's ETH balance won't cover at least this many more
	// months of upcoming pulls at current gas prices. Computed dynamically
	// from the active sub count + chain head — no static wei threshold to
	// keep in sync with usage. Default: 1. 0 disables the check.
	OperatorGasBufferMonths int

	// OperatorGasWarnInterval: dedup window for the gas-low webhook. While
	// the balance stays below threshold we re-fire at most once per
	// interval. Default: 6h.
	OperatorGasWarnInterval time.Duration

	// HTTP server.
	ListenAddr string

	// Admin token — required header value for /admin/* routes.
	AdminToken string
}

func Load() (*Config, error) {
	cfg := &Config{
		RPCURL:         os.Getenv("RPC_URL"),
		ContractAddr:   os.Getenv("CONTRACT_ADDR"),
		TokenAddr:      os.Getenv("TOKEN_ADDR"),
		OperatorKeyHex: os.Getenv("OPERATOR_KEY_HEX"),
		RedisURL:       os.Getenv("REDIS_URL"),
		WebhookURL:     os.Getenv("WEBHOOK_URL"),
		WebhookSecret:  os.Getenv("WEBHOOK_SECRET"),
		ListenAddr:     getenv("LISTEN_ADDR", ":8080"),
		AdminToken:     os.Getenv("ADMIN_TOKEN"),
	}

	chainID, err := strconv.ParseInt(getenv("CHAIN_ID", "8453"), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("CHAIN_ID: %w", err)
	}
	cfg.ChainID = chainID

	tick, err := time.ParseDuration(getenv("TICK_INTERVAL", "60s"))
	if err != nil {
		return nil, fmt.Errorf("TICK_INTERVAL: %w", err)
	}
	cfg.TickInterval = tick

	cfg.DunningBackoffs = []time.Duration{24 * time.Hour, 72 * time.Hour, 168 * time.Hour}

	low, err := strconv.Atoi(getenv("ALLOWANCE_LOW_MONTHS", "2"))
	if err != nil {
		return nil, fmt.Errorf("ALLOWANCE_LOW_MONTHS: %w", err)
	}
	cfg.AllowanceLowMonths = low

	buf, err := strconv.Atoi(getenv("OPERATOR_GAS_BUFFER_MONTHS", "1"))
	if err != nil {
		return nil, fmt.Errorf("OPERATOR_GAS_BUFFER_MONTHS: %w", err)
	}
	cfg.OperatorGasBufferMonths = buf
	warnInt, err := time.ParseDuration(getenv("OPERATOR_GAS_WARN_INTERVAL", "6h"))
	if err != nil {
		return nil, fmt.Errorf("OPERATOR_GAS_WARN_INTERVAL: %w", err)
	}
	cfg.OperatorGasWarnInterval = warnInt

	if cfg.RPCURL == "" || cfg.ContractAddr == "" || cfg.TokenAddr == "" || cfg.OperatorKeyHex == "" ||
		cfg.RedisURL == "" || cfg.AdminToken == "" {
		return nil, errors.New("missing required env: RPC_URL, CONTRACT_ADDR, TOKEN_ADDR, OPERATOR_KEY_HEX, REDIS_URL, ADMIN_TOKEN")
	}
	return cfg, nil
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
