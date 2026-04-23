package quant

// DefaultWarmupDays is the lookback prepended before each evaluation window
// for indicator warm-up. Docs reference: 進化計算引擎.md §1.1
const DefaultWarmupDays = 1200

// CrucibleWindow is one evaluation slice for GA fitness scoring.
// Bars includes a warmup prefix followed by eval bars, sorted time-ascending.
// EvalStartMs is the boundary: bars before it are warmup-only.
type CrucibleWindow struct {
	Label       string  // "6m" | "2y" | "5y" | "full"
	Weight      float64 // fitness weighting factor (all weights sum to 1.0)
	Bars        []Bar   // warmup prefix + eval bars, time-ascending; shares underlying array
	EvalStartMs int64   // first bar OpenTime that counts toward fitness scoring
}

// CrucibleResult holds per-window scoring output from the fitness function.
type CrucibleResult struct {
	Window string
	Score  float64
	ROI    float64
	MaxDD  float64
	Alpha  float64 // ROI_strategy - ROI_ghostDCA
}

// BuildCrucibleWindows slices allBars into four evaluation windows ordered
// short→long (matching the cascade short-circuit evaluation order).
// allBars must be sorted time-ascending.
// warmupDays controls the lookback prepended before each eval start for indicator warm-up.
// Docs reference: 進化計算引擎.md §1.1
func BuildCrucibleWindows(allBars []Bar, warmupDays int) []CrucibleWindow {
	if len(allBars) == 0 {
		return nil
	}

	const msPerDay = int64(24 * 60 * 60 * 1000)
	lastMs := allBars[len(allBars)-1].OpenTime
	warmupMs := int64(warmupDays) * msPerDay

	// findFirstIdx returns the index of the first bar with OpenTime >= targetMs.
	findFirstIdx := func(targetMs int64) int {
		for i, b := range allBars {
			if b.OpenTime >= targetMs {
				return i
			}
		}
		return len(allBars)
	}

	type spec struct {
		label  string
		weight float64
		days   int // 0 = use all history as eval
	}
	// Ordered short→long to match cascade short-circuit: 6m first, full last.
	specs := []spec{
		{"6m", 0.10, 183},
		{"2y", 0.20, 730},
		{"5y", 0.30, 1825},
		{"full", 0.40, 0},
	}

	var windows []CrucibleWindow
	for _, s := range specs {
		var evalStartMs int64
		if s.days == 0 {
			// "full" window: the entire history is the eval period.
			evalStartMs = allBars[0].OpenTime
		} else {
			evalStartMs = lastMs - int64(s.days)*msPerDay
			if evalStartMs <= allBars[0].OpenTime {
				evalStartMs = allBars[0].OpenTime
			}
		}

		evalIdx := findFirstIdx(evalStartMs)
		if evalIdx >= len(allBars) {
			// Insufficient data for this window; skip rather than panic.
			continue
		}

		// Warmup slice starts warmupDays before evalStartMs; clamped to bar[0].
		sliceStartIdx := findFirstIdx(evalStartMs - warmupMs)

		windows = append(windows, CrucibleWindow{
			Label:       s.label,
			Weight:      s.weight,
			Bars:        allBars[sliceStartIdx:], // slice shares the underlying array — zero-copy
			EvalStartMs: evalStartMs,
		})
	}
	return windows
}
