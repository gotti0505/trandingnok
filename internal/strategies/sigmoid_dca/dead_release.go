package sigmoid_dca

import "quantsaas/internal/quant"

// runRelease checks the ROI-based DeadStack → FloatStack release condition.
//
// When triggered it returns a RELEASE intent so the SaaS layer can update the
// ledger and write an audit log.  No TradeCommand is ever sent to the Agent for
// a release — it is a pure SaaS-side re-classification (docs §6.1).
//
// Action "RELEASE", Engine "MACRO", LotType "DEAD_STACK", QtyAsset = qty moved.
func runRelease(p quant.PortfolioState, spawn quant.SpawnPoint, price float64) *quant.TradeIntent {
	res := quant.TryReleaseDeadToFloat(p, spawn, price)
	if res.Released <= 0 {
		return nil
	}
	return &quant.TradeIntent{
		Action:   "RELEASE",
		Engine:   "MACRO",
		LotType:  "DEAD_STACK",
		QtyAsset: res.Released,
	}
}
