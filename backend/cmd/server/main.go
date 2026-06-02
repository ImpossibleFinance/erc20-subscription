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
	})
	a := api.New(rs, cfg.AdminToken, api.Deps{
		Chain:              cc,
		Webhook:            wh,
		TokenAddr:          common.HexToAddress(cfg.TokenAddr),
		AllowanceLowMonths: cfg.AllowanceLowMonths,
	})

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           a.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go sched.Run(ctx)
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
