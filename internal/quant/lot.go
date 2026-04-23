package quant

// ReleaseResult carries the updated stack quantities after a DeadStack release.
type ReleaseResult struct {
	DeadStack  float64 // reduced by release_qty
	FloatStack float64 // increased by release_qty
	Released   float64 // amount moved (0 when conditions not met)
}

// TryReleaseDeadToFloat checks the ROI-based release condition and moves a
// fraction of DeadStack into FloatStack when the threshold is reached.
//
// Release rule (docs §6.1):
//   - Trigger: portfolio ROI ≥ spawn.ROIReleaseTrigger (default 0.50 = 50%)
//   - Amount:  DeadStack × spawn.ReleaseRatio (default 0.20 = 20%)
//   - Nature:  pure SaaS-side ledger re-classification; no TradeCommand emitted
//   - Re-arm:  ROI must drop and rise again above trigger for another release
//
// The caller is responsible for writing an audit log entry when Released > 0.
func TryReleaseDeadToFloat(p PortfolioState, spawn SpawnPoint, price float64) ReleaseResult {
	res := ReleaseResult{
		DeadStack:  p.DeadStack,
		FloatStack: p.FloatStack,
	}

	if p.DeadStack <= 0 || price <= 0 || p.NAVInitial <= 0 {
		return res
	}

	nav := p.TotalUSDT + (p.DeadStack+p.FloatStack+p.ColdSealedStack)*price
	roi := (nav - p.NAVInitial) / p.NAVInitial

	if roi < spawn.ROIReleaseTrigger {
		return res
	}

	releaseQty := p.DeadStack * spawn.ReleaseRatio
	if releaseQty <= 0 {
		return res
	}

	res.DeadStack = p.DeadStack - releaseQty
	res.FloatStack = p.FloatStack + releaseQty
	res.Released = releaseQty
	return res
}
