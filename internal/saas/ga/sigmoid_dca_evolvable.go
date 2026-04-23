package ga

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"hash/fnv"
	"math"
	"math/rand"

	"quantsaas/internal/adapters/backtest"
	"quantsaas/internal/quant"
	"quantsaas/internal/strategies/sigmoid_dca"
	"quantsaas/internal/strategy"
)

const (
	fatalScore = -99999.0
	maxDDFatal = 0.88
	takerFee   = 0.001 // 0.1% Bitget taker fee — hardcoded; not subject to GA evolution
)

// SigmoidDCAEvolvable adapts the sigmoid_dca strategy to the EvolvableStrategy interface.
//
// Design note: this file lives in the ga package (not sigmoid_dca) to avoid
// import cycles:  ga → sigmoid_dca → quant  (no cycle).
// sigmoid_dca must never import ga.
type SigmoidDCAEvolvable struct {
	strat strategy.Strategy
}

// NewSigmoidDCAEvolvable returns a ready-to-use evolvable adapter.
func NewSigmoidDCAEvolvable() *SigmoidDCAEvolvable {
	return &SigmoidDCAEvolvable{strat: sigmoid_dca.New()}
}

func (e *SigmoidDCAEvolvable) StrategyID() string { return sigmoid_dca.ID }

// Sample draws a uniformly random Chromosome from the legal search space.
func (e *SigmoidDCAEvolvable) Sample(rng *rand.Rand) Gene {
	b := quant.HardBounds
	c := quant.Chromosome{
		DCADropPct:         b.DCADropPctMin + rng.Float64()*(b.DCADropPctMax-b.DCADropPctMin),
		SigmoidSensitivity: b.SigmoidSensitivityMin + rng.Float64()*(b.SigmoidSensitivityMax-b.SigmoidSensitivityMin),
		StopLossThreshold:  b.StopLossThresholdMin + rng.Float64()*(b.StopLossThresholdMax-b.StopLossThresholdMin),
	}
	quant.ClampChromosome(&c)
	return c
}

// geneSteps defines the per-field mutation step sizes used as Gaussian σ.
var geneSteps = quant.Chromosome{
	DCADropPct:         0.01,
	SigmoidSensitivity: 0.50,
	StopLossThreshold:  0.01,
}

// Mutate applies independent per-field Bernoulli-gated additive Gaussian noise.
// Each field mutates with probability prob; the mutation magnitude is
// NormFloat64() × fieldStep × scale.
func (e *SigmoidDCAEvolvable) Mutate(g Gene, prob, scale float64, rng *rand.Rand) Gene {
	c := g.(quant.Chromosome)
	if rng.Float64() < prob {
		c.DCADropPct += rng.NormFloat64() * geneSteps.DCADropPct * scale
	}
	if rng.Float64() < prob {
		c.SigmoidSensitivity += rng.NormFloat64() * geneSteps.SigmoidSensitivity * scale
	}
	if rng.Float64() < prob {
		c.StopLossThreshold += rng.NormFloat64() * geneSteps.StopLossThreshold * scale
	}
	quant.ClampChromosome(&c)
	return c
}

// Crossover performs uniform crossover: each field is independently drawn
// from p1 or p2 with 0.5 probability.
func (e *SigmoidDCAEvolvable) Crossover(p1, p2 Gene, rng *rand.Rand) Gene {
	c1, c2 := p1.(quant.Chromosome), p2.(quant.Chromosome)
	child := c1
	if rng.Float64() < 0.5 {
		child.DCADropPct = c2.DCADropPct
	}
	if rng.Float64() < 0.5 {
		child.SigmoidSensitivity = c2.SigmoidSensitivity
	}
	if rng.Float64() < 0.5 {
		child.StopLossThreshold = c2.StopLossThreshold
	}
	quant.ClampChromosome(&child)
	return child
}

// Fingerprint returns a FNV-1a-64 hash of the chromosome quantized to 1e-6
// precision.  Two chromosomes identical within 1e-6 must yield the same hash.
func (e *SigmoidDCAEvolvable) Fingerprint(g Gene) uint64 {
	c := g.(quant.Chromosome)
	h := fnv.New64a()
	buf := make([]byte, 8)
	for _, v := range []float64{c.DCADropPct, c.SigmoidSensitivity, c.StopLossThreshold} {
		quantized := math.Round(v*1e6) / 1e6
		binary.LittleEndian.PutUint64(buf, math.Float64bits(quantized))
		h.Write(buf)
	}
	return h.Sum64()
}

// Evaluate runs the multi-window crucible fitness assessment.
// Cascade order: 6m → 2y → 5y → full.
// A MaxDD ≥ 88% in any window is a fatal result: fatalScore is returned immediately
// and no further windows are evaluated (cascade short-circuit).
func (e *SigmoidDCAEvolvable) Evaluate(ctx context.Context, g Gene, plan EvaluablePlan) (float64, []quant.CrucibleResult) {
	c := g.(quant.Chromosome)
	var results []quant.CrucibleResult
	scoreTotal := 0.0

	for i, w := range plan.Windows {
		select {
		case <-ctx.Done():
			return fatalScore, results
		default:
		}

		baseline := plan.DCABaselines[i]

		m := backtest.RunBacktest(backtest.Config{
			Bars:        w.Bars,
			EvalStartMs: w.EvalStartMs,
			Chromosome:  c,
			SpawnPoint:  *plan.Spawn,
			LotStep:     plan.LotStep,
			LotMin:      plan.LotMin,
			TakerFee:    takerFee,
		}, e.strat)

		// Fatal: MaxDD ≥ 88% hard-disqualifies this individual.
		if m.MaxDrawdown >= maxDDFatal {
			results = append(results, quant.CrucibleResult{
				Window: w.Label,
				Score:  fatalScore,
				ROI:    m.ROI,
				MaxDD:  m.MaxDrawdown,
				Alpha:  m.ROI - baseline.ROI,
			})
			return fatalScore, results
		}

		alpha := m.ROI - baseline.ROI
		// Penalise excess drawdown relative to the passive DCA baseline.
		ddPenalty := 1.5 * math.Max(0, m.MaxDrawdown-baseline.MaxDrawdown)
		sliceScore := alpha - ddPenalty

		results = append(results, quant.CrucibleResult{
			Window: w.Label,
			Score:  sliceScore,
			ROI:    m.ROI,
			MaxDD:  m.MaxDrawdown,
			Alpha:  alpha,
		})
		scoreTotal += w.Weight * sliceScore
	}

	return scoreTotal, results
}

// sdcaParamPack is the on-disk JSON structure for a sigmoid_dca ParamPack.
// Mirrors the structure in internal/strategies/sigmoid_dca/params.go.
type sdcaParamPack struct {
	SpawnPoint quant.SpawnPoint `json:"spawn_point"`
	Config     quant.Chromosome `json:"sigmoid_dca_config"`
}

// DecodeElite decodes a Chromosome from the full ParamPack JSON blob stored in DB.
// On empty input or parse failure, returns DefaultSeedChromosome.
func (e *SigmoidDCAEvolvable) DecodeElite(raw []byte) Gene {
	if len(raw) == 0 {
		return quant.DefaultSeedChromosome
	}
	var pp sdcaParamPack
	if err := json.Unmarshal(raw, &pp); err != nil {
		return quant.DefaultSeedChromosome
	}
	quant.ClampChromosome(&pp.Config)
	return pp.Config
}

// EncodeResult serialises the winning Chromosome + SpawnPoint into a ParamPack JSON blob.
func (e *SigmoidDCAEvolvable) EncodeResult(g Gene, spawn *quant.SpawnPoint) ([]byte, error) {
	c := g.(quant.Chromosome)
	pp := sdcaParamPack{Config: c}
	if spawn != nil {
		pp.SpawnPoint = *spawn
	}
	return json.Marshal(pp)
}

// DecodeSpawnFromParamPack extracts just the SpawnPoint from a raw ParamPack JSON.
// Returns zero-value SpawnPoint on failure.
func DecodeSpawnFromParamPack(raw []byte) quant.SpawnPoint {
	var pp sdcaParamPack
	_ = json.Unmarshal(raw, &pp)
	return pp.SpawnPoint
}
