// cmd/saas is the SaaS cloud service entry point.
//
// Startup sequence (per 系統總體拓撲結構.md §6.1):
//  1. Load config
//  2. Connect Postgres (AutoMigrate) + Redis
//  3. Construct WebSocket Hub
//  4. Construct Instance Manager + Cron Scheduler
//  5. Register routes (GET /ws/agent)
//  6. Start scheduler + HTTP server
//  7. Graceful shutdown on SIGTERM / SIGINT
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"quantsaas/internal/saas/config"
	"quantsaas/internal/saas/cron"
	"quantsaas/internal/saas/instance"
	"quantsaas/internal/saas/store"
	"quantsaas/internal/saas/ws"
	"quantsaas/internal/strategies/sigmoid_dca"
)

func main() {
	logger, _ := zap.NewProduction()
	defer logger.Sync() //nolint:errcheck

	// ── Step 1: config ────────────────────────────────────────────────────────
	cfgPath := "config.yaml"
	if p := os.Getenv("CONFIG_PATH"); p != "" {
		cfgPath = p
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		logger.Fatal("config load failed", zap.String("path", cfgPath), zap.Error(err))
	}

	// ── Step 2: DB + Redis ────────────────────────────────────────────────────
	db, err := store.NewDB(cfg)
	if err != nil {
		logger.Fatal("database init failed", zap.Error(err))
	}
	redis := store.NewRedis(cfg)

	// ── Step 3: WebSocket Hub ─────────────────────────────────────────────────
	hub := ws.NewHub(cfg, db.DB, logger)

	// ── Step 4: Instance Manager + Cron Scheduler ─────────────────────────────
	strat := sigmoid_dca.New()
	mgr := instance.NewManager(db.DB, redis, hub, strat, logger)
	sched := cron.NewScheduler(db.DB, mgr, logger)

	// ── Step 5: Gin router ────────────────────────────────────────────────────
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/ws/agent", hub.HandleConnection)

	// ── Step 6: start scheduler + HTTP server ─────────────────────────────────
	sched.Start()

	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      r,
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 0, // disabled for long-lived WebSocket connections
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		logger.Info("SaaS server listening", zap.String("addr", addr))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("server error", zap.Error(err))
		}
	}()

	// ── Step 7: graceful shutdown ─────────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Info("shutdown signal received — stopping gracefully")
	sched.Stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("HTTP server shutdown error", zap.Error(err))
	}
	logger.Info("server stopped")
}
