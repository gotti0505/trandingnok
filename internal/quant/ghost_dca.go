package quant

import "time"

// GhostDCAConfig parameterises the passive DCA baseline used as the GA
// fitness benchmark.  Docs reference: 進化計算引擎.md §3 / plan §3G.
type GhostDCAConfig struct {
	InitialCapital float64 // seed USDT deployed on bar[0]
	MonthlyInject  float64 // USDT added at the start of each subsequent month
}

// GhostDCAResult holds the four fitness metrics returned by SimulateGhostDCA.
type GhostDCAResult struct {
	FinalEquity   float64 // portfolio value at last bar (BTC × close + 0 USDT)
	TotalInjected float64 // InitialCapital + all monthly injections
	MaxDrawdown   float64 // peak-to-trough NAV drawdown (0.0 – 1.0)
	ROI           float64 // Modified Dietz return (cash-flow-adjusted)
}

// SimulateGhostDCA runs the passive DCA strategy over the provided bar series
// and returns performance metrics.
//
// Strategy:
//   - Bar[0]: buy InitialCapital / close[0] BTC with all initial capital
//   - Each subsequent natural-month start: buy MonthlyInject / close[i] BTC
//   - All remaining USDT is always deployed — no cash is ever held idle
//
// ROI uses Modified Dietz to avoid distortion from injection jumps (docs §3).
func SimulateGhostDCA(cfg GhostDCAConfig, bars []Bar) GhostDCAResult {
	if len(bars) == 0 || cfg.InitialCapital <= 0 {
		return GhostDCAResult{}
	}

	btc := 0.0
	navCurve := make([]float64, 0, len(bars))

	// Cash-flow tracking for Modified Dietz
	type cashFlow struct {
		day    int // days from bar[0]
		amount float64
	}
	var flows []cashFlow
	totalDays := daysSince(bars[0].OpenTime, bars[len(bars)-1].OpenTime)

	// --- Bar 0: deploy initial capital ---
	if bars[0].Close > 0 {
		btc = cfg.InitialCapital / bars[0].Close
	}
	// The initial capital is the starting portfolio value, not a cash-flow for
	// Modified Dietz purposes.
	navCurve = append(navCurve, btc*bars[0].Close)

	prevMonth := monthOf(bars[0].OpenTime)

	for i := 1; i < len(bars); i++ {
		b := bars[i]
		if b.Close <= 0 {
			navCurve = append(navCurve, btc*bars[i-1].Close)
			continue
		}

		// Detect new month → inject
		m := monthOf(b.OpenTime)
		if m != prevMonth && cfg.MonthlyInject > 0 {
			prevMonth = m
			btc += cfg.MonthlyInject / b.Close
			daysIn := daysSince(bars[0].OpenTime, b.OpenTime)
			flows = append(flows, cashFlow{daysIn, cfg.MonthlyInject})
		}

		navCurve = append(navCurve, btc*b.Close)
	}

	final := btc * bars[len(bars)-1].Close

	// --- Modified Dietz ROI ---
	// ROI = (V_end − V_start − ΣF_i) / (V_start + Σ(F_i × w_i))
	// w_i = (TotalDays − day_i) / TotalDays
	vStart := cfg.InitialCapital
	sumF := 0.0
	weightedF := 0.0
	for _, f := range flows {
		sumF += f.amount
		w := 1.0
		if totalDays > 0 {
			w = float64(totalDays-f.day) / float64(totalDays)
		}
		weightedF += f.amount * w
	}
	denominator := vStart + weightedF
	roi := 0.0
	if denominator > 0 {
		roi = (final - vStart - sumF) / denominator
	}

	return GhostDCAResult{
		FinalEquity:   final,
		TotalInjected: vStart + sumF,
		MaxDrawdown:   MaxDrawdown(navCurve),
		ROI:           roi,
	}
}

// MaxDrawdown computes the maximum peak-to-trough relative drawdown of a NAV curve.
// Returns a value in [0, 1]; 0 means no drawdown observed.
func MaxDrawdown(nav []float64) float64 {
	if len(nav) == 0 {
		return 0
	}
	peak := nav[0]
	maxDD := 0.0
	for _, v := range nav {
		if v > peak {
			peak = v
		}
		if peak > 0 {
			dd := (peak - v) / peak
			if dd > maxDD {
				maxDD = dd
			}
		}
	}
	return maxDD
}

// monthOf returns a compact month key (year*12 + month) for bar-series
// month-boundary detection.
func monthOf(tsMillis int64) int {
	t := time.Unix(tsMillis/1000, 0).UTC()
	return t.Year()*12 + int(t.Month())
}

// daysSince returns the number of days between two millisecond timestamps.
func daysSince(fromMillis, toMillis int64) int {
	diff := toMillis - fromMillis
	if diff < 0 {
		diff = -diff
	}
	return int(diff / 1000 / 60 / 60 / 24)
}
