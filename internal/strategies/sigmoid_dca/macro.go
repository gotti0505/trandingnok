package sigmoid_dca

import "quantsaas/internal/quant"

// runMacro is a thin pass-through to quant.ComputeMacroDecision.
// It exists to keep step.go readable and to centralise the MacroInput construction.
func runMacro(
	price float64,
	p quant.PortfolioState,
	c quant.Chromosome,
	rt quant.RuntimeState,
	monthStr string,
) quant.MacroOutput {
	return quant.ComputeMacroDecision(quant.MacroInput{
		Price:     price,
		Portfolio: p,
		Params:    c,
		RT:        rt,
		MonthStr:  monthStr,
	})
}
