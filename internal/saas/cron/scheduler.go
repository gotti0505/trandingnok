// Package cron drives RUNNING strategy instances with a once-per-minute scan.
// Each scan launches a concurrent Tick goroutine per instance so slow instances
// don't block fast ones.
package cron

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"quantsaas/internal/saas/instance"
	"quantsaas/internal/saas/store"
)

// Scheduler owns the background goroutine that periodically scans for RUNNING
// instances and dispatches Tick calls.
type Scheduler struct {
	db      *gorm.DB
	manager *instance.Manager
	logger  *zap.Logger

	stopCh chan struct{}
	once   sync.Once
}

// NewScheduler constructs a Scheduler.
func NewScheduler(db *gorm.DB, manager *instance.Manager, logger *zap.Logger) *Scheduler {
	return &Scheduler{
		db:      db,
		manager: manager,
		logger:  logger,
		stopCh:  make(chan struct{}),
	}
}

// Start launches the background scan loop.  Non-blocking; call Stop to shut it down.
func (s *Scheduler) Start() {
	go s.loop()
	s.logger.Info("cron scheduler started — scanning every minute")
}

// Stop signals the scan loop to exit.  Safe to call multiple times.
func (s *Scheduler) Stop() {
	s.once.Do(func() {
		close(s.stopCh)
		s.logger.Info("cron scheduler stopped")
	})
}

// loop ticks every minute and calls scan.
func (s *Scheduler) loop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.scan()
		case <-s.stopCh:
			return
		}
	}
}

// scan fetches all RUNNING instances from DB and launches a concurrent Tick
// goroutine for each one.  The scan waits for all goroutines to finish before
// returning so that the next tick interval starts cleanly.
func (s *Scheduler) scan() {
	var instances []store.StrategyInstance
	if err := s.db.Where("status = ?", "RUNNING").Find(&instances).Error; err != nil {
		s.logger.Error("cron scan: DB query failed", zap.Error(err))
		return
	}
	if len(instances) == 0 {
		return
	}

	var wg sync.WaitGroup
	for _, inst := range instances {
		inst := inst // capture loop var for goroutine
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each tick gets 50 s to complete — well within the 60 s tick interval
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
			defer cancel()
			if err := s.manager.Tick(ctx, inst); err != nil {
				s.logger.Error("cron tick error",
					zap.Uint("instance_id", inst.ID),
					zap.Error(err),
				)
			}
		}()
	}
	wg.Wait()
}
