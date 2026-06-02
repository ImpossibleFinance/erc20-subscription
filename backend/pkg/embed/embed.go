// Package embed re-exports the types and constructors needed to run the
// subscription puller inside another Go binary, instead of as a standalone
// service.
//
// Use this when you'd rather not run the cmd/server binary as a sidecar and
// would prefer to share the host process's Redis client, RPC, and admin
// surface. Everything else (cmd/server, the Dockerfile, the HTTP API) keeps
// working — embedding is opt-in.
//
// Example wiring in your host's main.go:
//
//	subStore   := embed.NewRedisStore(myRedisClient)
//	subLocker  := embed.NewRedisLocker(myRedisClient)
//	subChain, _ := embed.NewChain(ctx, rpcURL, contractAddr, chainID, operatorKeyHex)
//	subSender  := embed.NewWebhookSender(loopbackURL, webhookSecret)
//	sched := embed.NewScheduler(subChain, subStore, subSender, embed.SchedulerConfig{
//	    Interval: 60 * time.Second,
//	    TokenAddr: tokenAddr,
//	    AllowanceLowMonths: 2,
//	    OperatorGasBufferMonths: 1,
//	    OperatorGasWarnInterval: 6 * time.Hour,
//	    Policy: embed.DunningPolicy{Backoffs: []time.Duration{24*time.Hour, 72*time.Hour, 168*time.Hour}},
//	    Locker: subLocker,
//	})
//	go sched.Run(ctx)
package embed

import (
	"github.com/redis/go-redis/v9"

	"github.com/impossiblefinance/erc20-subscription/backend/internal/chain"
	"github.com/impossiblefinance/erc20-subscription/backend/internal/dunning"
	"github.com/impossiblefinance/erc20-subscription/backend/internal/scheduler"
	"github.com/impossiblefinance/erc20-subscription/backend/internal/store"
	"github.com/impossiblefinance/erc20-subscription/backend/internal/webhooks"
)

// ---------- store ----------

type (
	Plan         = store.Plan
	Subscription = store.Subscription
	Store        = store.Store
)

const (
	StatusActive    = store.StatusActive
	StatusPastDue   = store.StatusPastDue
	StatusCancelled = store.StatusCancelled

	PeriodUnitDay   = store.PeriodUnitDay
	PeriodUnitMonth = store.PeriodUnitMonth
	PeriodUnitYear  = store.PeriodUnitYear
)

func NewRedisStore(c *redis.Client) *store.RedisStore { return store.NewRedisFromClient(c) }
func NewMemoryStore() *store.MemoryStore              { return store.NewMemory() }

// NewRedisLocker constructs the tick-level distributed lock used by the
// scheduler to safely run across multiple replicas.
func NewRedisLocker(c *redis.Client) *store.RedisLocker { return store.NewRedisLocker(c) }

func ValidPeriodUnit(u string) bool { return store.ValidPeriodUnit(u) }

// ---------- chain ----------

type Chain = chain.Client

// NewChain dials the RPC, parses the operator key, and returns a chain
// client wired for the given contract.
var NewChain = chain.New

// ---------- scheduler ----------

type (
	Scheduler       = scheduler.Scheduler
	SchedulerConfig = scheduler.Config
	Locker          = scheduler.Locker
	NoOpLocker      = scheduler.NoOpLocker
)

func NewScheduler(c *chain.Client, s store.Store, w *webhooks.Sender, cfg scheduler.Config) *scheduler.Scheduler {
	return scheduler.New(c, s, w, cfg)
}

// ---------- dunning ----------

type DunningPolicy = dunning.Policy

// ---------- webhooks ----------

type WebhookSender = webhooks.Sender

func NewWebhookSender(url, secret string) *webhooks.Sender {
	return webhooks.NewSender(url, secret)
}

const (
	EventCharged           = webhooks.EventCharged
	EventPaymentFailed     = webhooks.EventPaymentFailed
	EventCancelled         = webhooks.EventCancelled
	EventAllowanceLow      = webhooks.EventAllowanceLow
	EventAllowanceRequired = webhooks.EventAllowanceRequired
	EventProratedCharge    = webhooks.EventProratedCharge
	EventOperatorGasLow    = webhooks.EventOperatorGasLow
	EventOperatorTxStuck   = webhooks.EventOperatorTxStuck
)
