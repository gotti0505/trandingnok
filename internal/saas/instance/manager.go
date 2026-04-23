// Package instance manages the lifecycle and Tick execution of strategy instances.
//
// State machine:
//
//	STOPPED  → RUNNING  (Start)
//	RUNNING  → STOPPED  (Stop)
//	any      → DELETED  (Delete)
//	RUNNING  → ERROR    (Tick returns a non-transient error)
//
// Tick() is the ONLY place Step() is called inside the SaaS process.
// It mirrors exactly the call made by the backtest adapter — no if-isBacktest
// branches anywhere in this file.
package instance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"quantsaas/internal/quant"
	"quantsaas/internal/saas/ga"
	"quantsaas/internal/saas/store"
	"quantsaas/internal/strategy"
)

// Hub is the minimal interface Manager needs to dispatch commands to LocalAgents.
// The concrete implementation (WebSocket Hub) is wired at cmd/saas startup.
type Hub interface {
	// SendToAgent delivers payload to the agent authenticated under userID.
	// Returns true when the agent is connected; false when offline.
	SendToAgent(userID uint, payload []byte) bool
}

// TradeCommand is the instruction sent to a LocalAgent over WebSocket.
// Docs reference: 系統總體拓撲結構.md §5.3
type TradeCommand struct {
	ClientOrderID string `json:"client_order_id"`
	Action        string `json:"action"`                // BUY | SELL
	Engine        string `json:"engine"`                // MACRO | MICRO
	Symbol        string `json:"symbol"`
	AmountUSDT    string `json:"amount_usdt,omitempty"` // BUY only
	QtyAsset      string `json:"qty_asset,omitempty"`   // SELL only
	LotType       string `json:"lot_type"`
}

// wsEnvelope wraps a TradeCommand in the WebSocket framing layer.
type wsEnvelope struct {
	Type    string      `json:"type"`
	Payload interface{} `json:"payload"`
}

// Manager handles instance lifecycle transitions and per-instance Tick execution.
type Manager struct {
	db     *gorm.DB
	redis  *store.RedisClient
	hub    Hub
	strat  strategy.Strategy // same Step() implementation used by backtest
	logger *zap.Logger
}

// NewManager constructs a Manager.
// strat must be the live strategy (e.g. sigmoid_dca.New()) — the same instance
// used in backtest to guarantee strategy isomorphism.
func NewManager(
	db *gorm.DB,
	redis *store.RedisClient,
	hub Hub,
	strat strategy.Strategy,
	logger *zap.Logger,
) *Manager {
	return &Manager{db: db, redis: redis, hub: hub, strat: strat, logger: logger}
}

// ─── State machine ────────────────────────────────────────────────────────────

// Start transitions a STOPPED instance to RUNNING.
func (m *Manager) Start(ctx context.Context, instanceID uint) error {
	res := m.db.WithContext(ctx).
		Model(&store.StrategyInstance{}).
		Where("id = ? AND status = ?", instanceID, "STOPPED").
		Update("status", "RUNNING")
	if res.Error != nil {
		return fmt.Errorf("Start: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("Start: instance %d is not in STOPPED state", instanceID)
	}
	return nil
}

// Stop transitions a RUNNING instance to STOPPED.
func (m *Manager) Stop(ctx context.Context, instanceID uint) error {
	res := m.db.WithContext(ctx).
		Model(&store.StrategyInstance{}).
		Where("id = ? AND status = ?", instanceID, "RUNNING").
		Update("status", "STOPPED")
	if res.Error != nil {
		return fmt.Errorf("Stop: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("Stop: instance %d is not in RUNNING state", instanceID)
	}
	return nil
}

// Delete marks an instance as DELETED regardless of current status.
func (m *Manager) Delete(ctx context.Context, instanceID uint) error {
	if err := m.db.WithContext(ctx).
		Model(&store.StrategyInstance{}).
		Where("id = ?", instanceID).
		Update("status", "DELETED").Error; err != nil {
		return fmt.Errorf("Delete: %w", err)
	}
	return nil
}

// markError moves a RUNNING instance to ERROR and records the cause.
func (m *Manager) markError(instanceID uint, cause error) {
	_ = m.db.
		Model(&store.StrategyInstance{}).
		Where("id = ? AND status = ?", instanceID, "RUNNING").
		Update("status", "ERROR").Error
	m.logger.Error("instance transitioned to ERROR",
		zap.Uint("instance_id", instanceID),
		zap.Error(cause),
	)
}

// ─── Tick ─────────────────────────────────────────────────────────────────────

// Tick executes one cron tick for inst.
// It implements the 10-step pipeline defined in 系統總體拓撲結構.md §6.2.
// Transient failures (network, agent offline) return nil so the cron retries
// on the next minute; persistent failures return a non-nil error that causes
// the instance to transition to ERROR.
// Safe to call concurrently for different instances.
func (m *Manager) Tick(ctx context.Context, inst store.StrategyInstance) (retErr error) {
	defer func() {
		if retErr != nil {
			m.markError(inst.ID, retErr)
		}
	}()

	// ── Step 1: idempotent bucket dedup check ─────────────────────────────────
	// Fetch latest bars from exchange public API; skip if the latest completed
	// bar has already been processed by this instance.
	bars, err := fetchLatestBars(ctx, inst.Symbol, inst.Interval, 300)
	if err != nil {
		m.logger.Warn("tick: fetch bars failed — will retry next minute",
			zap.Uint("instance_id", inst.ID), zap.Error(err))
		return nil // transient
	}
	if len(bars) < 2 {
		m.logger.Warn("tick: not enough bars",
			zap.Uint("instance_id", inst.ID), zap.Int("count", len(bars)))
		return nil
	}
	// After sort-ascending: bars[len-1] = forming candle, bars[len-2] = latest completed
	latestCompleted := bars[len(bars)-2]
	latestBarTime := time.UnixMilli(latestCompleted.OpenTime).UTC()

	// ── Step 2: load PortfolioState and RuntimeState ──────────────────────────
	dbPortfolio, err := m.loadOrCreatePortfolio(ctx, inst.ID)
	if err != nil {
		return fmt.Errorf("tick step2 portfolio: %w", err)
	}

	// Idempotency guard: same bar already processed → skip
	if !dbPortfolio.LastProcessedBarTime.IsZero() &&
		!latestBarTime.After(dbPortfolio.LastProcessedBarTime) {
		return nil
	}

	var dbRuntime store.RuntimeState
	rtResult := m.db.WithContext(ctx).
		Where("instance_id = ?", inst.ID).
		First(&dbRuntime)
	if rtResult.Error != nil && !errors.Is(rtResult.Error, gorm.ErrRecordNotFound) {
		return fmt.Errorf("tick step2 runtime: %w", rtResult.Error)
	}
	runtimeIsNew := errors.Is(rtResult.Error, gorm.ErrRecordNotFound)

	// ── Step 3: load current champion params from Redis / DB ──────────────────
	chromosome, spawnPoint, err := m.loadChampionParams(ctx, inst)
	if err != nil {
		return fmt.Errorf("tick step3 champion: %w", err)
	}

	// ── Step 4: ACL outer ring — extract price series from []Bar ─────────────
	closes := make([]float64, len(bars))
	highs := make([]float64, len(bars))
	lows := make([]float64, len(bars))
	timestamps := make([]int64, len(bars))
	for i, b := range bars {
		closes[i] = b.Close
		highs[i] = b.High
		lows[i] = b.Low
		timestamps[i] = b.OpenTime
	}

	// ── Step 5: build StrategyInput ───────────────────────────────────────────
	portfolio := quant.PortfolioState{
		TotalUSDT:       dbPortfolio.USDTBalance,
		ReserveFloor:    spawnPoint.ReserveFloor,
		SpendableUSDT:   clamp0(dbPortfolio.USDTBalance - spawnPoint.ReserveFloor),
		DeadStack:       dbPortfolio.DeadBTC,
		FloatStack:      dbPortfolio.FloatBTC,
		ColdSealedStack: dbPortfolio.ColdSealedBTC,
		NAVInitial:      spawnPoint.InitialCapital,
	}

	var runtime quant.RuntimeState
	if !runtimeIsNew && dbRuntime.Payload != "" && dbRuntime.Payload != "{}" {
		if err := json.Unmarshal([]byte(dbRuntime.Payload), &runtime); err != nil {
			// corrupted state: log and start fresh — do not abort the tick
			m.logger.Warn("tick: runtime JSON corrupt — resetting state",
				zap.Uint("instance_id", inst.ID), zap.Error(err))
			runtime = quant.RuntimeState{}
		}
	}

	input := strategy.StrategyInput{
		Closes:     closes,
		Highs:      highs,
		Lows:       lows,
		Timestamps: timestamps,
		Portfolio:  portfolio,
		Runtime:    runtime,
		Params:     chromosome,
		SpawnPoint: spawnPoint,
	}

	// ── Step 6: call Step() — only caller in the entire SaaS process ──────────
	output := m.strat.Step(input)

	// ── Step 7: persist updated RuntimeState ─────────────────────────────────
	if err := m.upsertRuntime(ctx, inst.ID, output.Runtime, &dbRuntime, runtimeIsNew); err != nil {
		return fmt.Errorf("tick step7 runtime persist: %w", err)
	}

	// ── Step 8: handle DeadStack release intent (ledger-only, no Agent cmd) ───
	// Release = SaaS-side reclassification of BTC from DeadStack → FloatStack.
	// No TradeCommand issued; AuditLog written on every successful release.
	for _, intent := range output.Intents {
		if intent.Action == "RELEASE" {
			m.handleRelease(ctx, inst.ID, intent)
		}
	}

	// ── Step 9: translate macro/micro intents to TradeCommands and dispatch ───
	nowMs := time.Now().UnixMilli()
	for _, intent := range output.Intents {
		if intent.Action == "RELEASE" {
			continue
		}
		cmd := intentToCommand(inst, intent, nowMs)
		if err := m.dispatchCommand(ctx, inst, cmd); err != nil {
			// Log but don't abort: remaining intents should still be attempted
			m.logger.Error("tick step9: dispatch failed",
				zap.Uint("instance_id", inst.ID),
				zap.String("client_order_id", cmd.ClientOrderID),
				zap.Error(err),
			)
		}
	}

	// ── Step 10: advance LastProcessedBarTime ─────────────────────────────────
	if err := m.db.WithContext(ctx).
		Model(dbPortfolio).
		Update("last_processed_bar_time", latestBarTime).Error; err != nil {
		return fmt.Errorf("tick step10 bar time: %w", err)
	}

	return nil
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

// loadOrCreatePortfolio fetches an existing PortfolioState or creates a zero-value
// row for a brand-new instance (first tick before any DeltaReport arrives).
func (m *Manager) loadOrCreatePortfolio(ctx context.Context, instanceID uint) (*store.PortfolioState, error) {
	var ps store.PortfolioState
	err := m.db.WithContext(ctx).Where("instance_id = ?", instanceID).First(&ps).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		ps = store.PortfolioState{InstanceID: instanceID}
		if err := m.db.WithContext(ctx).Create(&ps).Error; err != nil {
			return nil, fmt.Errorf("create portfolio: %w", err)
		}
		return &ps, nil
	}
	if err != nil {
		return nil, err
	}
	return &ps, nil
}

// loadChampionParams loads the chromosome and spawn point for the instance.
// It first checks the Redis champion cache (key: champion:{templateID}),
// then falls back to DB. If no champion exists yet, it decodes from the
// instance's own ParamPack (the params at the time the instance was created).
func (m *Manager) loadChampionParams(
	ctx context.Context,
	inst store.StrategyInstance,
) (quant.Chromosome, quant.SpawnPoint, error) {
	evolvable := ga.NewSigmoidDCAEvolvable()

	decode := func(paramPackJSON string) (quant.Chromosome, quant.SpawnPoint) {
		raw := []byte(paramPackJSON)
		chrom := evolvable.DecodeElite(raw).(quant.Chromosome)
		spawn := ga.DecodeSpawnFromParamPack(raw)
		return chrom, spawn
	}

	// Redis cache hit
	cacheKey := fmt.Sprintf("champion:%d", inst.TemplateID)
	if raw, err := m.redis.Get(ctx, cacheKey); err == nil && raw != "" {
		var champion store.GeneRecord
		if json.Unmarshal([]byte(raw), &champion) == nil {
			chrom, spawn := decode(champion.ParamPack)
			return chrom, spawn, nil
		}
	}

	// DB lookup
	var champion store.GeneRecord
	err := m.db.WithContext(ctx).
		Where("strategy_id = ? AND role = ?", inst.TemplateID, "champion").
		Order("created_at desc").
		First(&champion).Error

	if errors.Is(err, gorm.ErrRecordNotFound) {
		// No champion yet — fall back to the instance's own birth-time params
		chrom, spawn := decode(inst.ParamPack)
		return chrom, spawn, nil
	}
	if err != nil {
		return quant.Chromosome{}, quant.SpawnPoint{}, err
	}

	chrom, spawn := decode(champion.ParamPack)
	return chrom, spawn, nil
}

// upsertRuntime creates or overwrites the RuntimeState row for this instance.
func (m *Manager) upsertRuntime(
	ctx context.Context,
	instanceID uint,
	rt quant.RuntimeState,
	existing *store.RuntimeState,
	isNew bool,
) error {
	payload, err := json.Marshal(rt)
	if err != nil {
		return fmt.Errorf("marshal runtime: %w", err)
	}
	if isNew {
		row := store.RuntimeState{InstanceID: instanceID, Payload: string(payload)}
		return m.db.WithContext(ctx).Create(&row).Error
	}
	return m.db.WithContext(ctx).
		Model(existing).
		Update("payload", string(payload)).Error
}

// handleRelease applies a RELEASE intent as a ledger reclassification on the SaaS
// side: DeadBTC decreases, FloatBTC increases by the same amount.
// No TradeCommand is emitted. An AuditLog entry is always written on success.
func (m *Manager) handleRelease(ctx context.Context, instanceID uint, intent quant.TradeIntent) {
	qty := intent.QtyAsset
	if qty <= 0 {
		return
	}
	err := m.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var ps store.PortfolioState
		if err := tx.Where("instance_id = ?", instanceID).First(&ps).Error; err != nil {
			return err
		}
		if qty > ps.DeadBTC {
			qty = ps.DeadBTC // cap at available dead stack
		}
		ps.DeadBTC -= qty
		ps.FloatBTC += qty
		if err := tx.Save(&ps).Error; err != nil {
			return err
		}
		auditPayload, _ := json.Marshal(map[string]interface{}{
			"action":       "RELEASE",
			"released_btc": qty,
		})
		return tx.Create(&store.AuditLog{
			InstanceID: instanceID,
			EventType:  "dead_release",
			Payload:    string(auditPayload),
		}).Error
	})
	if err != nil {
		m.logger.Error("tick: RELEASE ledger transaction failed",
			zap.Uint("instance_id", instanceID),
			zap.Float64("qty", qty),
			zap.Error(err),
		)
	}
}

// dispatchCommand persists a pending SpotExecution record then forwards the
// TradeCommand to the Agent via WebSocket Hub.
// If the Agent is offline, the execution record remains in DB as "pending"
// and the tick will re-evaluate on the next candle.
func (m *Manager) dispatchCommand(ctx context.Context, inst store.StrategyInstance, cmd TradeCommand) error {
	raw, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshal command: %w", err)
	}

	exec := store.SpotExecution{
		InstanceID:    inst.ID,
		ClientOrderID: cmd.ClientOrderID,
		Status:        "pending",
		RawPayload:    string(raw),
	}
	if err := m.db.WithContext(ctx).Create(&exec).Error; err != nil {
		return fmt.Errorf("create SpotExecution: %w", err)
	}

	envelope, _ := json.Marshal(wsEnvelope{Type: "command", Payload: cmd})
	if !m.hub.SendToAgent(inst.UserID, envelope) {
		m.logger.Warn("tick: agent offline — command queued in DB for next tick",
			zap.Uint("instance_id", inst.ID),
			zap.String("client_order_id", cmd.ClientOrderID),
		)
	}
	return nil
}

// intentToCommand translates a TradeIntent into a TradeCommand.
// client_order_id format: inst{id}-{engine}-{ts}  (docs §5.3)
func intentToCommand(inst store.StrategyInstance, intent quant.TradeIntent, nowMs int64) TradeCommand {
	cmd := TradeCommand{
		ClientOrderID: fmt.Sprintf("inst%d-%s-%d", inst.ID, intent.Engine, nowMs),
		Action:        intent.Action,
		Engine:        intent.Engine,
		Symbol:        inst.Symbol,
		LotType:       intent.LotType,
	}
	switch intent.Action {
	case "BUY":
		cmd.AmountUSDT = strconv.FormatFloat(intent.AmountUSDT, 'f', 2, 64)
	case "SELL":
		cmd.QtyAsset = strconv.FormatFloat(intent.QtyAsset, 'f', 8, 64)
	}
	return cmd
}

func clamp0(v float64) float64 {
	if v < 0 {
		return 0
	}
	return v
}

// ─── K-line fetcher (Bitget public API) ──────────────────────────────────────

// bitgetCandleResp is the JSON envelope returned by Bitget v2 spot candles.
type bitgetCandleResp struct {
	Code string     `json:"code"`
	Msg  string     `json:"msg"`
	Data [][]string `json:"data"`
}

// fetchLatestBars fetches `limit` bars from Bitget's public candles endpoint.
// The returned slice is sorted oldest → newest.
// "BTC/USDT" is automatically normalised to the Bitget symbol "BTCUSDT".
// On any network or API error the caller should skip the tick and retry.
func fetchLatestBars(ctx context.Context, symbol, interval string, limit int) ([]quant.Bar, error) {
	sym := strings.ReplaceAll(symbol, "/", "")
	url := fmt.Sprintf(
		"https://api.bitget.com/api/v2/spot/market/candles?symbol=%s&granularity=%s&limit=%d",
		sym, interval, limit,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	var result bitgetCandleResp
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("unmarshal candles: %w", err)
	}
	if result.Code != "00000" {
		return nil, fmt.Errorf("bitget error %s: %s", result.Code, result.Msg)
	}

	// Each row: [ts, open, high, low, close, baseVol, quoteVol, usdtVol]
	bars := make([]quant.Bar, 0, len(result.Data))
	for _, row := range result.Data {
		if len(row) < 5 {
			continue
		}
		ts, _ := strconv.ParseInt(row[0], 10, 64)
		open, _ := strconv.ParseFloat(row[1], 64)
		high, _ := strconv.ParseFloat(row[2], 64)
		low, _ := strconv.ParseFloat(row[3], 64)
		close_, _ := strconv.ParseFloat(row[4], 64)
		bars = append(bars, quant.Bar{
			OpenTime: ts,
			Open:     open,
			High:     high,
			Low:      low,
			Close:    close_,
		})
	}

	// Guarantee ascending order regardless of Bitget's response ordering
	sort.Slice(bars, func(i, j int) bool { return bars[i].OpenTime < bars[j].OpenTime })
	return bars, nil
}
