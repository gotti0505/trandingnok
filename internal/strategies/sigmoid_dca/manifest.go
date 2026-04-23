package sigmoid_dca

// Strategy identity constants — referenced by SaaS when registering the template.
const (
	ID          = "sigmoid_dca_v1"
	Name        = "Sigmoid DCA"
	Version     = "1.0.0"
	IsSpot      = true
	Description = "EMA200-gated bull/bear state machine: macro DCA accumulation in bear, " +
		"Sigmoid dynamic-balance profit-taking in bull."
)
