package ws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"quantsaas/internal/saas/store"
)

// storedCommand is the minimal subset of TradeCommand needed for lot accounting.
// It is decoded from SpotExecution.RawPayload.
type storedCommand struct {
	Action  string `json:"action"`   // BUY | SELL
	Engine  string `json:"engine"`   // MACRO | MICRO
	Symbol  string `json:"symbol"`
	LotType string `json:"lot_type"` // DEAD_STACK | FLOATING
}

// processDeltaReport applies a DeltaReport to the database inside one transaction.
//
// When Execution is nil or ClientOrderID is empty (Agent reconnect initial snapshot),
// only the balance snapshot is updated — no lot records are touched.
//
// Full report flow:
//  1. Find pending SpotExecution by ClientOrderID → mark filled
//  2. Update PortfolioState BTC tracking (DeadBTC or FloatBTC) per LotType
//  3. Write TradeRecord
//  4. Update USDTBalance snapshot for all user instances
//  5. Write AuditLog
func processDeltaReport(
	ctx context.Context,
	db *gorm.DB,
	logger *zap.Logger,
	userID uint,
	payload deltaReportPayload,
) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		hasExecution := payload.Execution != nil && payload.Execution.ClientOrderID != ""

		if !hasExecution {
			// Initial reconnect snapshot: only sync balance, no lot updates.
			return updateBalances(tx, logger, userID, payload.Balances)
		}

		// ── Step 1: locate the pending SpotExecution ──────────────────────────
		var exec store.SpotExecution
		err := tx.Where("client_order_id = ? AND status = ?",
			payload.Execution.ClientOrderID, "pending").
			First(&exec).Error

		if errors.Is(err, gorm.ErrRecordNotFound) {
			// Idempotent: already processed (e.g. duplicate report). Still sync balances.
			logger.Warn("processDeltaReport: SpotExecution not found or already filled",
				zap.String("client_order_id", payload.Execution.ClientOrderID))
			return updateBalances(tx, logger, userID, payload.Balances)
		}
		if err != nil {
			return fmt.Errorf("find SpotExecution: %w", err)
		}

		// ── Step 1b: mark filled ──────────────────────────────────────────────
		if err := tx.Model(&exec).Update("status", "filled").Error; err != nil {
			return fmt.Errorf("update SpotExecution status: %w", err)
		}

		// ── Step 2: decode stored TradeCommand to get LotType ─────────────────
		var cmd storedCommand
		if err := json.Unmarshal([]byte(exec.RawPayload), &cmd); err != nil {
			return fmt.Errorf("decode SpotExecution RawPayload: %w", err)
		}

		// ── Step 2b: update PortfolioState BTC tracking ───────────────────────
		var ps store.PortfolioState
		if err := tx.Where("instance_id = ?", exec.InstanceID).First(&ps).Error; err != nil {
			return fmt.Errorf("load PortfolioState for instance %d: %w", exec.InstanceID, err)
		}

		qty := payload.Execution.FilledQty
		switch cmd.LotType {
		case "DEAD_STACK":
			if cmd.Action == "BUY" {
				ps.DeadBTC += qty
			} else {
				ps.DeadBTC = maxZero(ps.DeadBTC - qty)
			}
		case "FLOATING":
			if cmd.Action == "BUY" {
				ps.FloatBTC += qty
			} else {
				ps.FloatBTC = maxZero(ps.FloatBTC - qty)
			}
		default:
			logger.Warn("processDeltaReport: unknown LotType — skipping BTC update",
				zap.String("lot_type", cmd.LotType),
				zap.Uint("instance_id", exec.InstanceID))
		}

		if err := tx.Save(&ps).Error; err != nil {
			return fmt.Errorf("save PortfolioState: %w", err)
		}

		// ── Step 3: write TradeRecord ─────────────────────────────────────────
		trade := store.TradeRecord{
			InstanceID:    exec.InstanceID,
			ClientOrderID: payload.Execution.ClientOrderID,
			Action:        cmd.Action,
			Engine:        cmd.Engine,
			Symbol:        cmd.Symbol,
			FilledQty:     payload.Execution.FilledQty,
			FilledPrice:   payload.Execution.FilledPrice,
			Fee:           payload.Execution.Fee,
		}
		if err := tx.Create(&trade).Error; err != nil {
			return fmt.Errorf("create TradeRecord: %w", err)
		}

		// ── Step 4: sync balance snapshot for all user instances ──────────────
		if err := updateBalances(tx, logger, userID, payload.Balances); err != nil {
			return err
		}

		// ── Step 5: write AuditLog ────────────────────────────────────────────
		auditData, _ := json.Marshal(map[string]interface{}{
			"client_order_id": payload.Execution.ClientOrderID,
			"filled_qty":      payload.Execution.FilledQty,
			"filled_price":    payload.Execution.FilledPrice,
			"fee":             payload.Execution.Fee,
			"lot_type":        cmd.LotType,
			"action":          cmd.Action,
		})
		if err := tx.Create(&store.AuditLog{
			InstanceID: exec.InstanceID,
			EventType:  "delta_report",
			Payload:    string(auditData),
		}).Error; err != nil {
			return fmt.Errorf("create AuditLog: %w", err)
		}

		return nil
	})
}

// updateBalances sets USDTBalance on every non-deleted instance belonging to userID.
// The exchange balance snapshot is the source of truth for USDT (Available only,
// since Frozen represents already-committed orders).
func updateBalances(
	tx *gorm.DB,
	logger *zap.Logger,
	userID uint,
	balances []balanceMsg,
) error {
	if len(balances) == 0 {
		return nil
	}

	var usdtAvailable float64
	for _, b := range balances {
		if b.Asset == "USDT" {
			usdtAvailable = b.Available
			break
		}
	}

	var instances []store.StrategyInstance
	if err := tx.Where("user_id = ? AND status != ?", userID, "DELETED").
		Find(&instances).Error; err != nil {
		return fmt.Errorf("load instances for user %d: %w", userID, err)
	}

	for _, inst := range instances {
		res := tx.Model(&store.PortfolioState{}).
			Where("instance_id = ?", inst.ID).
			Update("usdt_balance", usdtAvailable)
		if res.Error != nil {
			logger.Warn("updateBalances: failed to update instance portfolio",
				zap.Uint("instance_id", inst.ID), zap.Error(res.Error))
		}
	}
	return nil
}

func maxZero(v float64) float64 {
	if v < 0 {
		return 0
	}
	return v
}
