package quant

import "math"

const (
	ema200Period     = 200
	ema200SlopeBars  = 10    // compare current EMA200 vs this many ticks ago for slope
	quietSigmaThresh = 0.015 // 1.5% — docs §2.3
	ema200HistMax    = 11    // keep last 11 values to support 10-bar slope look-back
)

// MarketStateResult is the output of ComputeMarketState.
// Docs reference: 策略數學引擎.md §2
type MarketStateResult struct {
	State                  string  // "QUIET" | "BEAR" | "BULL"
	IsQuiet                bool    // true when σ24h < 1.5%
	BetaMultiplier         float64 // reserved for future market-regime amplification; always 1.0 now
	TimeDilationMultiplier float64 // reserved for macro engine time-scaling; always 1.0 now
}

// MarketStateInput bundles all data needed by ComputeMarketState.
type MarketStateInput struct {
	Closes []float64 // full close series, oldest → newest
	Highs  []float64 // full high series, same length as Closes
	Lows   []float64 // full low series, same length as Closes
	Price  float64   // current close (= Closes[len-1])
	RT     RuntimeState
}

// ComputeMarketState classifies market regime and returns the updated RuntimeState.
//
// Algorithm (docs §2.1 priority order):
//  1. QUIET  — σ24h < 1.5%  → all trading suspended this tick
//  2. BEAR   — price < EMA200
//  3. BULL   — price > EMA200 AND EMA200 slope > 0
//  4. default to BEAR when price ≥ EMA200 but slope ≤ 0 (ambiguous recovery)
func ComputeMarketState(in MarketStateInput) (MarketStateResult, RuntimeState) {
	rt := in.RT

	// --- Update EMA200 (docs §2.2) ---
	var ema200 float64
	if rt.EMA200 == 0 {
		ema200 = EMA(in.Closes, ema200Period)
	} else {
		ema200 = UpdateEMA(rt.EMA200, in.Price, ema200Period)
	}
	hist := append(append([]float64(nil), rt.EMA200History...), ema200)
	if len(hist) > ema200HistMax {
		hist = hist[len(hist)-ema200HistMax:]
	}
	rt.EMA200 = ema200
	rt.EMA200History = hist

	// --- σ24h (docs §2.3): range of the last 24 highs/lows ---
	sigma := sigma24h(in.Highs, in.Lows)

	res := MarketStateResult{
		BetaMultiplier:         1.0,
		TimeDilationMultiplier: 1.0,
	}

	if sigma < quietSigmaThresh {
		res.State = "QUIET"
		res.IsQuiet = true
		return res, rt
	}

	// EMA200 slope: current value minus the value ema200SlopeBars ticks ago
	slope := ema200Slope(hist)

	if in.Price < ema200 {
		res.State = "BEAR"
	} else if in.Price > ema200 && slope > 0 {
		res.State = "BULL"
	} else {
		// price ≥ EMA200 but slope ≤ 0 — recovery not confirmed, treat conservatively
		res.State = "BEAR"
	}
	return res, rt
}

// sigma24h computes (High24h − Low24h) / Low24h over the last 24 bars.
// Returns 0 when data is insufficient.
func sigma24h(highs, lows []float64) float64 {
	n := len(highs)
	if n == 0 || len(lows) != n {
		return 0
	}
	start := n - 24
	if start < 0 {
		start = 0
	}
	h24 := -math.MaxFloat64
	l24 := math.MaxFloat64
	for i := start; i < n; i++ {
		if highs[i] > h24 {
			h24 = highs[i]
		}
		if lows[i] < l24 {
			l24 = lows[i]
		}
	}
	if l24 <= 0 {
		return 0
	}
	return (h24 - l24) / l24
}

// ema200Slope returns EMA200[now] − EMA200[ema200SlopeBars ago].
// When history is shorter than ema200SlopeBars+1 it uses whatever is available.
func ema200Slope(hist []float64) float64 {
	m := len(hist)
	if m < 2 {
		return 0
	}
	lookback := ema200SlopeBars
	if lookback >= m {
		lookback = m - 1
	}
	return hist[m-1] - hist[m-1-lookback]
}
