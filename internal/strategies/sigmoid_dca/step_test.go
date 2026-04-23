package sigmoid_dca

import (
	"testing"

	"quantsaas/internal/quant"
	"quantsaas/internal/strategy"
)

// buildInput constructs a minimal StrategyInput for testing.
// price is the latest close; all bars are flat by default.
func buildInput(nBars int, price float64) strategy.StrategyInput {
	closes := make([]float64, nBars)
	highs := make([]float64, nBars)
	lows := make([]float64, nBars)
	timestamps := make([]int64, nBars)
	baseTS := int64(1_700_000_000_000) // arbitrary epoch ms
	for i := range closes {
		closes[i] = price
		highs[i] = price * 1.005
		lows[i] = price * 0.995
		timestamps[i] = baseTS + int64(i)*3600_000 // hourly bars
	}
	return strategy.StrategyInput{
		Closes:     closes,
		Highs:      highs,
		Lows:       lows,
		Timestamps: timestamps,
		Portfolio: quant.PortfolioState{
			TotalUSDT:     10_000,
			ReserveFloor:  1_000,
			SpendableUSDT: 9_000,
			DeadStack:     0,
			FloatStack:    0,
			NAVInitial:    10_000,
		},
		Params:     quant.DefaultSeedChromosome,
		SpawnPoint: quant.SpawnPoint{ROIReleaseTrigger: 0.50, ReleaseRatio: 0.20},
	}
}

// TestStepPureFunction verifies determinism: same input → same output.
func TestStepPureFunction(t *testing.T) {
	s := New()
	in := buildInput(300, 30_000)
	out1 := s.Step(in)
	out2 := s.Step(in)
	if len(out1.Intents) != len(out2.Intents) {
		t.Fatalf("non-deterministic: call1 intents=%d call2 intents=%d",
			len(out1.Intents), len(out2.Intents))
	}
}

// TestStepTooFewBars ensures a no-op is returned when history is shorter than minBarsRequired.
func TestStepTooFewBars(t *testing.T) {
	s := New()
	in := buildInput(minBarsRequired-1, 30_000)
	out := s.Step(in)
	if len(out.Intents) != 0 {
		t.Fatalf("expected empty intents for short history, got %d", len(out.Intents))
	}
}

// TestStepQuietMarketNoTrades confirms that a flat (quiet) market produces no intents.
func TestStepQuietMarketNoTrades(t *testing.T) {
	s := New()
	// Flat price → σ24h ≈ 0 → QUIET
	in := buildInput(300, 30_000)
	out := s.Step(in)
	if len(out.Intents) != 0 {
		t.Fatalf("expected no trades in quiet market, got %d intent(s): %+v",
			len(out.Intents), out.Intents)
	}
}

// TestStepBearMarketMacroBuy confirms a BUY/DEAD_STACK intent in bear conditions.
func TestStepBearMarketMacroBuy(t *testing.T) {
	s := New()
	// Simulate bear: recent price well below EMA200 seed, with real volatility.
	in := buildInput(300, 30_000)
	// Spike highs/lows to break quiet threshold
	for i := range in.Highs {
		in.Highs[i] = 30_000 * 1.05
		in.Lows[i] = 30_000 * 0.95
	}
	// Force bear: set price below the (seeded) EMA200 by dropping last candle
	in.Closes[len(in.Closes)-1] = 20_000
	in.Highs[len(in.Highs)-1] = 20_000 * 1.05
	in.Lows[len(in.Lows)-1] = 20_000 * 0.95

	// Seed EMA200 at 30000 so current price 20000 < EMA200 → BEAR
	in.Runtime.EMA200 = 30_000

	// Set month high so tier 1 fires: monthHigh * (1 - 0.10) = 27000 > 20000
	in.Runtime.MonthHigh = 30_000
	in.Runtime.MonthStr = quant.MonthKey(in.Timestamps[len(in.Timestamps)-1])

	out := s.Step(in)

	var buyCount int
	for _, intent := range out.Intents {
		if intent.Action == "BUY" && intent.Engine == "MACRO" {
			buyCount++
		}
	}
	if buyCount == 0 {
		t.Fatalf("expected at least one MACRO BUY in bear market, got intents: %+v", out.Intents)
	}
}

// TestStepStopLossLiquidatesFloat verifies stop-loss sells all FloatStack.
func TestStepStopLossLiquidatesFloat(t *testing.T) {
	s := New()
	in := buildInput(300, 10_000) // price crashed
	for i := range in.Highs {
		in.Highs[i] = 10_000 * 1.05
		in.Lows[i] = 10_000 * 0.95
	}
	// Bull state: price > EMA200 with upward slope
	in.Runtime.EMA200 = 9_000
	in.Runtime.EMA200History = []float64{8_500, 8_600, 8_700, 8_800, 8_900, 9_000, 9_000, 9_000, 9_000, 9_000, 9_000}
	in.Portfolio.FloatStack = 0.5
	in.Portfolio.NAVInitial = 50_000 // started much higher → ROI ≈ -80% < -20% threshold
	in.Params.StopLossThreshold = -0.20

	out := s.Step(in)

	var sellAll bool
	for _, intent := range out.Intents {
		if intent.Action == "SELL" && intent.QtyAsset == 0.5 {
			sellAll = true
		}
	}
	if !sellAll {
		t.Fatalf("expected full FloatStack liquidation on stop-loss, got: %+v", out.Intents)
	}
}

// TestParseParamPackRoundTrip checks JSON encode/decode of the param pack.
func TestParseParamPackRoundTrip(t *testing.T) {
	raw := `{
		"spawn_point": {"roi_release_trigger": 0.5, "release_ratio": 0.2},
		"sigmoid_dca_config": {"dca_drop_pct": 0.15, "sigmoid_sensitivity": 7.0, "stop_loss_threshold": -0.25}
	}`
	c, sp := ParseParamPack(raw)
	if c.DCADropPct != 0.15 {
		t.Errorf("DCADropPct want 0.15, got %v", c.DCADropPct)
	}
	if sp.ROIReleaseTrigger != 0.5 {
		t.Errorf("ROIReleaseTrigger want 0.5, got %v", sp.ROIReleaseTrigger)
	}
}

// TestParseParamPackBadJSON falls back to default seed on invalid JSON.
func TestParseParamPackBadJSON(t *testing.T) {
	c, _ := ParseParamPack("not json")
	if c.DCADropPct != quant.DefaultSeedChromosome.DCADropPct {
		t.Errorf("expected default fallback, got %+v", c)
	}
}
