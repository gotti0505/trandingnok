package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"quantsaas/internal/saas/epoch"
	"quantsaas/internal/saas/store"
)

// EvolutionHandler bundles all evolution-related HTTP handlers.
// It is wired into the router by the Phase 9 routes layer.
type EvolutionHandler struct {
	epochSvc *epoch.EpochService
	db       *gorm.DB
	redis    *store.RedisClient
}

// NewEvolutionHandler constructs the handler.
func NewEvolutionHandler(svc *epoch.EpochService, db *gorm.DB, redis *store.RedisClient) *EvolutionHandler {
	return &EvolutionHandler{epochSvc: svc, db: db, redis: redis}
}

// --- POST /api/v1/evolution/tasks ---

// CreateTask parses the request, delegates to EpochService, and returns the task record.
// Only available when app_role = lab | dev (enforced by middleware upstream).
func (h *EvolutionHandler) CreateTask(c *gin.Context) {
	var req epoch.CreateTaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}

	task, err := h.epochSvc.CreateAndRunTask(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isConflict(err) {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"task": task})
}

// isConflict detects the "already running" error by message prefix.
func isConflict(err error) bool {
	return err != nil && len(err.Error()) > 0 && err.Error()[:7] == "another"
}

// --- GET /api/v1/evolution/tasks ---

// ListTasks returns the current task status and all challenger GeneRecords.
func (h *EvolutionHandler) ListTasks(c *gin.Context) {
	current := h.epochSvc.GetCurrentTask()

	// Fetch all challenger records ordered newest-first.
	var challengers []store.GeneRecord
	h.db.Where("role = ?", "challenger").Order("created_at desc").Find(&challengers)

	type challengerItem struct {
		ID          uint               `json:"id"`
		CreatedAt   time.Time          `json:"created_at"`
		ScoreTotal  float64            `json:"score_total"`
		MaxDrawdown float64            `json:"max_drawdown"`
		WindowScores map[string]float64 `json:"window_scores"`
	}

	items := make([]challengerItem, 0, len(challengers))
	for _, ch := range challengers {
		ws := make(map[string]float64)
		_ = json.Unmarshal([]byte(ch.WindowScores), &ws)
		items = append(items, challengerItem{
			ID:           ch.ID,
			CreatedAt:    ch.CreatedAt,
			ScoreTotal:   ch.ScoreTotal,
			MaxDrawdown:  ch.MaxDrawdown,
			WindowScores: ws,
		})
	}

	resp := gin.H{"challengers": items}
	if current != nil {
		resp["running"] = current.Status == "running"
		resp["current_generation"] = current.CurrentGeneration
		resp["best_score_so_far"] = current.BestScore
		resp["status"] = current.Status
	} else {
		resp["running"] = false
	}

	c.JSON(http.StatusOK, resp)
}

// --- POST /api/v1/evolution/tasks/:taskID/promote ---

// PromoteChallenger promotes the challenger produced by :taskID to champion.
// Executed in a DB transaction: current champion → retired, challenger → champion.
// Redis champion cache is invalidated after the transaction.
func (h *EvolutionHandler) PromoteChallenger(c *gin.Context) {
	taskIDStr := c.Param("taskID")
	taskID, err := strconv.ParseUint(taskIDStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid taskID"})
		return
	}

	// Find the challenger produced by this task.
	var challenger store.GeneRecord
	if err := h.db.Where("task_id = ? AND role = ?", taskID, "challenger").
		First(&challenger).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("no challenger found for task %d", taskID)})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// DB transaction: retire current champion, promote challenger.
	err = h.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&store.GeneRecord{}).
			Where("strategy_id = ? AND role = ?", challenger.StrategyID, "champion").
			Update("role", "retired").Error; err != nil {
			return fmt.Errorf("retire champion: %w", err)
		}
		if err := tx.Model(&challenger).Update("role", "champion").Error; err != nil {
			return fmt.Errorf("promote challenger: %w", err)
		}
		return nil
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Invalidate Redis champion cache so the next Tick picks up the new champion.
	cacheKey := fmt.Sprintf("champion:%d", challenger.StrategyID)
	_ = h.redis.Del(c.Request.Context(), cacheKey)

	c.JSON(http.StatusOK, gin.H{"message": "challenger promoted to champion", "champion_id": challenger.ID})
}

// --- GET /api/v1/genome/champion ---

// GetChampion returns the current champion gene record.
// Checks Redis cache first (key: champion:{strategyID}); falls back to DB on miss.
func (h *EvolutionHandler) GetChampion(c *gin.Context) {
	strategyIDStr := c.Query("strategy_id")
	strategyID, err := strconv.ParseUint(strategyIDStr, 10, 64)
	if err != nil || strategyID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "strategy_id query param required"})
		return
	}

	cacheKey := fmt.Sprintf("champion:%d", strategyID)

	// Cache hit — return the cached JSON blob directly.
	if raw, err := h.redis.Get(c.Request.Context(), cacheKey); err == nil && raw != "" {
		var champion store.GeneRecord
		if json.Unmarshal([]byte(raw), &champion) == nil {
			c.JSON(http.StatusOK, gin.H{"champion": champion})
			return
		}
	}

	// Cache miss — load from DB.
	var champion store.GeneRecord
	if err := h.db.Where("strategy_id = ? AND role = ?", strategyID, "champion").
		Order("created_at desc").
		First(&champion).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "no champion found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Write to Redis cache (5 minute TTL).
	if data, err := json.Marshal(champion); err == nil {
		_ = h.redis.Set(c.Request.Context(), cacheKey, string(data), 5*time.Minute)
	}

	c.JSON(http.StatusOK, gin.H{"champion": champion})
}
