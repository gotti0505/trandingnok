package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"go.uber.org/zap"

	"quantsaas/internal/agent/config"
	"quantsaas/internal/agent/exchange"
	"quantsaas/internal/agent/ws"
)

func main() {
	logger, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "init logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync() //nolint:errcheck

	cfgPath := "config.agent.yaml"
	if len(os.Args) > 1 {
		cfgPath = os.Args[1]
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		logger.Fatal("load config", zap.String("path", cfgPath), zap.Error(err))
	}

	bitget := exchange.NewBitgetClient(cfg)
	client := ws.NewAgentClient(cfg, bitget, logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	logger.Info("QuantSaaS LocalAgent starting",
		zap.String("saas_url", cfg.SaaSURL),
		zap.String("exchange", cfg.Exchange.Name),
		zap.Bool("sandbox", cfg.Exchange.Sandbox),
	)

	client.Run(ctx)
	logger.Info("agent stopped cleanly")
}
