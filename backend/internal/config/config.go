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

	// Hosted-checkout sessions (see docs/checkout-sessions.md).
	SessionTTL          time.Duration // lifetime of a pending session; clamped [5m, 60m]
	ChallengeFreshness  time.Duration // max age of the EIP-191 `Issued:` line
	MinConfirmations    int           // block depth required before accepting a tx
	ChallengePrefix     string        // first line of the EIP-191 message; integrator-brandable
	ApprovePeriodsHint  int           // how many periods the page is told to target on approve (default 12)
	SessionSweepInterval time.Duration // how often to expire pending sessions past TTL
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

	sessTTL, err := time.ParseDuration(getenv("SESSION_TTL", "15m"))
	if err != nil {
		return nil, fmt.Errorf("SESSION_TTL: %w", err)
	}
	if sessTTL < 5*time.Minute {
		sessTTL = 5 * time.Minute
	}
	if sessTTL > 60*time.Minute {
		sessTTL = 60 * time.Minute
	}
	cfg.SessionTTL = sessTTL

	chFresh, err := time.ParseDuration(getenv("CHALLENGE_FRESHNESS", "10m"))
	if err != nil {
		return nil, fmt.Errorf("CHALLENGE_FRESHNESS: %w", err)
	}
	cfg.ChallengeFreshness = chFresh

	// Default confirmations: 1 on common testnets, 3 elsewhere. Operators
	// pick the explicit value via env for anything serious.
	defaultConfs := "3"
	switch cfg.ChainID {
	case 84532, 11155111, 421614, 11155420: // base-sepolia, eth-sepolia, arb-sepolia, op-sepolia
		defaultConfs = "1"
	}
	confs, err := strconv.Atoi(getenv("MIN_CONFIRMATIONS", defaultConfs))
	if err != nil {
		return nil, fmt.Errorf("MIN_CONFIRMATIONS: %w", err)
	}
	cfg.MinConfirmations = confs

	cfg.ChallengePrefix = getenv("CHALLENGE_PREFIX", "erc20-subscription checkout")

	hint, err := strconv.Atoi(getenv("APPROVE_PERIODS_HINT", "12"))
	if err != nil {
		return nil, fmt.Errorf("APPROVE_PERIODS_HINT: %w", err)
	}
	cfg.ApprovePeriodsHint = hint

	sweep, err := time.ParseDuration(getenv("SESSION_SWEEP_INTERVAL", "1m"))
	if err != nil {
		return nil, fmt.Errorf("SESSION_SWEEP_INTERVAL: %w", err)
	}
	cfg.SessionSweepInterval = sweep

	return cfg, nil
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
