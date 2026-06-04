// Command server boots the subscription backend: HTTP API + scheduler. Config
// from env (see internal/config/config.go).
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/impossiblefinance/erc20-subscription/backend/internal/api"
	"github.com/impossiblefinance/erc20-subscription/backend/internal/chain"
	"github.com/impossiblefinance/erc20-subscription/backend/internal/config"
	"github.com/impossiblefinance/erc20-subscription/backend/internal/dunning"
	"github.com/impossiblefinance/erc20-subscription/backend/internal/scheduler"
	"github.com/impossiblefinance/erc20-subscription/backend/internal/store"
	"github.com/impossiblefinance/erc20-subscription/backend/internal/webhooks"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rs, err := store.NewRedis(cfg.RedisURL)
	if err != nil {
		log.Fatalf("redis: %v", err)
	}
	// Shared Redis client → store + scheduler tick-lock.
	locker := store.NewRedisLocker(rs.Client())

	cc, err := chain.New(ctx, cfg.RPCURL, cfg.ContractAddr, cfg.ChainID, cfg.OperatorKeyHex)
	if err != nil {
		log.Fatalf("chain: %v", err)
	}
	log.Printf("operator address: %s", cc.OperatorAddress().Hex())
	log.Printf("contract address: %s", cc.Contract().Hex())

	wh := webhooks.NewSender(cfg.WebhookURL, cfg.WebhookSecret)
	sched := scheduler.New(cc, rs, wh, scheduler.Config{
		Policy:                  dunning.Policy{Backoffs: cfg.DunningBackoffs},
		Interval:                cfg.TickInterval,
		TokenAddr:               common.HexToAddress(cfg.TokenAddr),
		AllowanceLowMonths:      cfg.AllowanceLowMonths,
		OperatorGasBufferMonths: cfg.OperatorGasBufferMonths,
		OperatorGasWarnInterval: cfg.OperatorGasWarnInterval,
		Locker:                  locker,
	})
	log.Printf("scheduler instance: %s", sched.InstanceID())

	// Resolve treasury + token metadata from the contract / token at boot.
	// Treasury is the destination the hosted-checkout page tells the user to
	// send the first month's funds to; reading from the contract avoids an
	// env var drifting out of sync with the immutable.
	treasuryAddr, err := cc.Treasury(ctx)
	if err != nil {
		log.Fatalf("read treasury from contract: %v", err)
	}
	log.Printf("treasury address: %s", treasuryAddr.Hex())
	tokenSym, tokenDec, err := cc.TokenInfo(ctx, common.HexToAddress(cfg.TokenAddr))
	if err != nil {
		log.Printf("token info read failed (continuing with defaults): %v", err)
		tokenSym, tokenDec = "USDC", 6
	}

	a := api.New(rs, cfg.AdminToken, api.Deps{
		Chain:              cc,
		Webhook:            wh,
		TokenAddr:          common.HexToAddress(cfg.TokenAddr),
		TreasuryAddr:       treasuryAddr,
		TokenSymbol:        tokenSym,
		TokenDecimals:      tokenDec,
		AllowanceLowMonths: cfg.AllowanceLowMonths,
		ChallengePrefix:    cfg.ChallengePrefix,
		ChallengeFreshness: cfg.ChallengeFreshness,
		SessionTTL:         cfg.SessionTTL,
		MinConfirmations:   uint64(cfg.MinConfirmations),
		ApprovePeriodsHint: cfg.ApprovePeriodsHint,
	})

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           a.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go sched.Run(ctx)
	go a.RunSessionSweeper(ctx, cfg.SessionSweepInterval)
	go func() {
		log.Printf("HTTP listening on %s", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	<-sigs
	log.Printf("shutting down")
	cancel()
	shutdownCtx, c2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer c2()
	_ = srv.Shutdown(shutdownCtx)
}
