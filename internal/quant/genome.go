package quant

// Chromosome holds the three GA-evolvable parameters for the sigmoid-DCA strategy.
// Bounds are enforced by ClampChromosome after every crossover or mutation.
// Docs reference: 策略數學引擎.md §7.1
type Chromosome struct {
	DCADropPct         float64 `json:"dca_drop_pct"`         // [0.05, 0.30] — base drop threshold for DCA tiers
	SigmoidSensitivity float64 `json:"sigmoid_sensitivity"`  // [1.0, 10.0]  — k in Sigmoid formula
	StopLossThreshold  float64 `json:"stop_loss_threshold"`  // [-0.30, -0.10] — negative; e.g. -0.20
}

// SpawnPoint holds birth-time parameters that are fixed for the lifetime of an
// instance.  They are not part of the chromosome and never enter GA evolution.
// Docs reference: 策略數學引擎.md §7.2
type SpawnPoint struct {
	BaseCurrency      string  `json:"base_currency"`       // e.g. "USDT"
	TradingPair       string  `json:"trading_pair"`        // e.g. "BTC/USDT"
	InitialCapital    float64 `json:"initial_capital"`     // user-set seed capital (USDT)
	ReserveFloor      float64 `json:"reserve_floor"`       // safety baseline — never spent
	ROIReleaseTrigger float64 `json:"roi_release_trigger"` // 0.50 → release at +50% ROI
	ReleaseRatio      float64 `json:"release_ratio"`       // 0.20 → release 20% of DeadStack
	MonthlyInject     float64 `json:"monthly_inject"`      // monthly top-up (USDT, may be 0)
}

// HardBounds defines the legal [min, max] range for each evolvable field.
// ClampChromosome uses these to repair out-of-range values after mutation.
var HardBounds = struct {
	DCADropPctMin, DCADropPctMax               float64
	SigmoidSensitivityMin, SigmoidSensitivityMax float64
	StopLossThresholdMin, StopLossThresholdMax  float64
}{
	DCADropPctMin: 0.05, DCADropPctMax: 0.30,
	SigmoidSensitivityMin: 1.0, SigmoidSensitivityMax: 10.0,
	// -0.30 is the lower bound (larger loss allowed), -0.10 is the upper bound (tighter stop)
	StopLossThresholdMin: -0.30, StopLossThresholdMax: -0.10,
}

// DefaultSeedChromosome is the product-default champion seed used when GA
// cold-starts or when JSON decoding of a stored chromosome fails.
var DefaultSeedChromosome = Chromosome{
	DCADropPct:         0.10,
	SigmoidSensitivity: 5.0,
	StopLossThreshold:  -0.20,
}

// ClampChromosome repairs all fields to their legal ranges.
// Must be called after every GA crossover or mutation.
func ClampChromosome(c *Chromosome) {
	c.DCADropPct = ClipFloat64(c.DCADropPct, HardBounds.DCADropPctMin, HardBounds.DCADropPctMax)
	c.SigmoidSensitivity = ClipFloat64(c.SigmoidSensitivity, HardBounds.SigmoidSensitivityMin, HardBounds.SigmoidSensitivityMax)
	// stop_loss_threshold is negative; -0.30 < -0.10, so min=-0.30, max=-0.10
	c.StopLossThreshold = ClipFloat64(c.StopLossThreshold, HardBounds.StopLossThresholdMin, HardBounds.StopLossThresholdMax)
}
