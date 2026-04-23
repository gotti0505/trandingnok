package sigmoid_dca

import "quantsaas/internal/quant"

// runMicro is a thin pass-through to quant.ComputeMicroDecision.
func runMicro(
	price float64,
	ema200 float64,
	p quant.PortfolioState,
	c quant.Chromosome,
	rt quant.RuntimeState,
) quant.MicroOutput {
	return quant.ComputeMicroDecision(quant.MicroInput{
		Price:     price,
		EMA200:    ema200,
		Portfolio: p,
		Params:    c,
		RT:        rt,
	})
}
