// Package epoch manages the evolution task lifecycle: creating tasks, resolving
// the spawn_mode, launching RunEpoch asynchronously, and reporting progress.
package epoch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"quantsaas/internal/quant"
	"quantsaas/internal/saas/ga"
	"quantsaas/internal/saas/store"
)

// CreateTaskRequest is the parsed body of POST /api/v1/evolution/tasks.
type CreateTaskRequest struct {
	StrategyID     uint              `json:"strategy_id"`
	Symbol         string            `json:"symbol"`
	Interval       string            `json:"interval"`
	PopSize        int               `json:"pop_size"`
	MaxGenerations int               `json:"max_generations"`
	SpawnMode      string            `json:"spawn_mode"` // inherit | random_once | manual
	SpawnPoint     *quant.SpawnPoint `json:"spawn_point"` // only for spawn_mode="manual"
	TestMode       bool              `json:"test_mode"`   // Pop=10, Gen=3 — for quick testing
	LotStep        float64           `json:"lot_step"`
	LotMin         float64           `json:"lot_min"`
}

// EpochService manages the single active evolution task.
type EpochService struct {
	db     *gorm.DB
	engine *ga.EvolutionEngine
	logger *zap.Logger

	mu          sync.Mutex
	activeTask  *store.EvolutionTask
	cancelFn    context.CancelFunc
}

// NewEpochService constructs a service.
func NewEpochService(db *gorm.DB, engine *ga.EvolutionEngine, logger *zap.Logger) *EpochService {
	return &EpochService{db: db, engine: engine, logger: logger}
}

// CreateAndRunTask validates the request, creates an EvolutionTask DB record,
// and asynchronously launches RunEpoch.  Returns the task record immediately
// so the caller can respond without waiting.
func (s *EpochService) CreateAndRunTask(req CreateTaskRequest) (*store.EvolutionTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.activeTask != nil && s.activeTask.Status == "running" {
		return nil, errors.New("another evolution task is already running")
	}

	// Resolve spawn point according to spawn_mode.
	spawn, err := s.resolveSpawnPoint(req)
	if err != nil {
		return nil, fmt.Errorf("CreateAndRunTask resolveSpawnPoint: %w", err)
	}

	// Apply test-mode overrides.
	if req.TestMode {
		req.PopSize = 10
		req.MaxGenerations = 3
	}

	// Defaults for empty fields.
	if req.SpawnMode == "" {
		req.SpawnMode = "inherit"
	}
	if req.LotStep == 0 {
		req.LotStep = 0.00001
	}
	if req.LotMin == 0 {
		req.LotMin = 0.00001
	}

	cfgJSON, _ := json.Marshal(req)

	task := &store.EvolutionTask{
		StrategyID: req.StrategyID,
		Status:     "running",
		Config:     string(cfgJSON),
	}
	if err := s.db.Create(task).Error; err != nil {
		return nil, fmt.Errorf("CreateAndRunTask create task: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.activeTask = task
	s.cancelFn = cancel

	// Launch epoch in background goroutine.
	go s.runEpoch(ctx, task.ID, req, spawn)

	return task, nil
}

// GetCurrentTask returns the active task snapshot (nil if idle).
func (s *EpochService) GetCurrentTask() *store.EvolutionTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeTask
}

// CancelActiveTask cancels the running epoch if one is active.
func (s *EpochService) CancelActiveTask() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancelFn != nil {
		s.cancelFn()
	}
}

// runEpoch is the goroutine body that drives the GA engine and handles status updates.
func (s *EpochService) runEpoch(ctx context.Context, taskID uint, req CreateTaskRequest, spawn *quant.SpawnPoint) {
	logger := s.logger.With(zap.Uint("task_id", taskID))
	logger.Info("epoch started")

	updateTask := func(fields map[string]any) {
		if err := s.db.Model(&store.EvolutionTask{}).Where("id = ?", taskID).Updates(fields).Error; err != nil {
			logger.Warn("failed to update task progress", zap.Error(err))
		}
	}

	maxGen := req.MaxGenerations
	if maxGen <= 0 {
		maxGen = 25
	}

	onProgress := func(gen int, best, mutProb, mutScale float64) {
		progress := float64(gen+1) / float64(maxGen)
		updateTask(map[string]any{
			"current_generation": gen + 1,
			"best_score":         best,
			"progress":           progress,
		})
		logger.Debug("epoch progress",
			zap.Int("gen", gen+1),
			zap.Float64("best", best),
			zap.Float64("mut_prob", mutProb),
			zap.Float64("mut_scale", mutScale),
		)
	}

	result, err := s.engine.RunEpoch(ctx, ga.EpochConfig{
		StrategyID:         req.StrategyID,
		TaskID:             taskID,
		Symbol:             req.Symbol,
		Interval:           req.Interval,
		PopSize:            req.PopSize,
		MaxGenerations:     req.MaxGenerations,
		LotStep:            req.LotStep,
		LotMin:             req.LotMin,
		SpawnPointOverride: spawn,
		OnProgress:         onProgress,
	})

	if err != nil {
		logger.Error("epoch failed", zap.Error(err))
		updateTask(map[string]any{
			"status":   "failed",
			"progress": 1.0,
			"error_msg": err.Error(),
		})
		s.mu.Lock()
		s.activeTask.Status = "failed"
		s.mu.Unlock()
		return
	}

	logger.Info("epoch completed",
		zap.Float64("best_score", result.BestScore),
		zap.Int("generations", result.Generations),
		zap.Uint("challenger_id", result.ChallengerID),
	)

	updateTask(map[string]any{
		"status":             "done",
		"progress":           1.0,
		"current_generation": result.Generations,
		"best_score":         result.BestScore,
	})

	s.mu.Lock()
	s.activeTask.Status = "done"
	s.mu.Unlock()
}

// resolveSpawnPoint maps spawn_mode to a concrete SpawnPoint.
func (s *EpochService) resolveSpawnPoint(req CreateTaskRequest) (*quant.SpawnPoint, error) {
	switch req.SpawnMode {
	case "manual":
		if req.SpawnPoint == nil {
			return nil, errors.New("spawn_mode=manual requires spawn_point in request body")
		}
		return req.SpawnPoint, nil

	case "random_once":
		sp := randomSpawnPoint()
		return &sp, nil

	default: // "inherit" or empty
		var champion store.GeneRecord
		err := s.db.Where("strategy_id = ? AND role = ?", req.StrategyID, "champion").
			Order("created_at desc").
			First(&champion).Error

		if err == nil {
			// Decode SpawnPoint from the champion's ParamPack JSON.
			sp := ga.DecodeSpawnFromParamPack([]byte(champion.ParamPack))
			if sp.InitialCapital > 0 {
				return &sp, nil
			}
		}
		// Fallback to system default.
		sp := defaultSpawnPoint()
		return &sp, nil
	}
}

// defaultSpawnPoint returns the product-level default SpawnPoint used when
// there is no champion in the database.
func defaultSpawnPoint() quant.SpawnPoint {
	return quant.SpawnPoint{
		BaseCurrency:      "USDT",
		TradingPair:       "BTC/USDT",
		InitialCapital:    1000,
		ReserveFloor:      50,
		ROIReleaseTrigger: 0.50,
		ReleaseRatio:      0.20,
		MonthlyInject:     0,
	}
}

// randomSpawnPoint samples a random SpawnPoint for spawn_mode="random_once".
// The randomised parameters are the policy scalars; the currency identifiers
// stay fixed.
func randomSpawnPoint() quant.SpawnPoint {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	return quant.SpawnPoint{
		BaseCurrency:      "USDT",
		TradingPair:       "BTC/USDT",
		InitialCapital:    1000,                                          // fixed
		ReserveFloor:      float64(50 + rng.Intn(250)),                  // [50, 300)
		ROIReleaseTrigger: 0.30 + rng.Float64()*0.70,                    // [0.30, 1.00)
		ReleaseRatio:      0.10 + rng.Float64()*0.30,                    // [0.10, 0.40)
		MonthlyInject:     0,
	}
}
