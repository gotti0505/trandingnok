// Package ga implements the Genetic Algorithm evolution engine and the
// EvolvableStrategy interface that decouples the engine from concrete strategies.
// Docs reference: 進化計算引擎.md §8
package ga

import (
	"context"
	"math/rand"

	"quantsaas/internal/quant"
)

// Gene is an opaque chromosome carrier.
// The engine holds and passes Gene values without reading internal fields.
// All chromosome-aware operations go through EvolvableStrategy.
type Gene = any

// DCABaseline holds Ghost DCA pre-computed results for one crucible window.
// Computed once at Epoch start; shared read-only across all Evaluate calls.
type DCABaseline struct {
	FinalEquity   float64
	TotalInjected float64
	MaxDrawdown   float64
	ROI           float64
}

// EvaluablePlan is the read-only context built once at Epoch start.
// It is passed unchanged to every Evaluate call within the same epoch.
// Docs reference: 進化計算引擎.md §8.2
type EvaluablePlan struct {
	Pair         string
	TemplateName string
	Spawn        *quant.SpawnPoint
	LotStep      float64
	LotMin       float64
	Windows      []quant.CrucibleWindow // sorted short→long (matches cascade order)
	DCABaselines []DCABaseline          // index-aligned with Windows; pre-computed at epoch start
}

// EpochResult is returned after RunEpoch completes successfully.
type EpochResult struct {
	BestScore    float64
	Generations  int
	ChallengerID uint // DB ID of the GeneRecord written as "challenger"
}

// EvolvableStrategy is the 8-verb interface between the GA engine and a concrete strategy.
// The engine is fully strategy-agnostic — it never reads chromosome field names or types.
// Docs reference: 進化計算引擎.md §8.1
type EvolvableStrategy interface {
	// StrategyID returns the strategy's unique identifier string (e.g. "sigmoid_dca_v1").
	StrategyID() string

	// Sample returns a uniformly random Gene drawn from the legal search space.
	Sample(rng *rand.Rand) Gene

	// Mutate applies per-dimension Bernoulli-gated additive Gaussian noise.
	// prob is the per-dimension mutation probability; scale multiplies the step size.
	Mutate(g Gene, prob, scale float64, rng *rand.Rand) Gene

	// Crossover performs uniform crossover on two parent genes, producing one child.
	// Each dimension is independently drawn from p1 or p2 with 0.5 probability.
	Crossover(p1, p2 Gene, rng *rand.Rand) Gene

	// Fingerprint returns a FNV-1a-64 hash of the gene quantized to 1e-6 precision.
	// Identical genes (within 1e-6 of each dimension) must produce the same fingerprint.
	Fingerprint(g Gene) uint64

	// Evaluate runs the multi-window crucible fitness assessment.
	// Windows in plan are in cascade order (short→long); a fatal window short-circuits.
	// Returns (scoreTotal, perWindowResults). Fatal genes return (fatalScore, partial).
	Evaluate(ctx context.Context, g Gene, plan EvaluablePlan) (float64, []quant.CrucibleResult)

	// DecodeElite decodes a Chromosome from the raw DB ParamPack JSON blob.
	// Returns DefaultSeedChromosome when raw is empty or fails to parse.
	DecodeElite(raw []byte) Gene

	// EncodeResult serialises the winning Gene and SpawnPoint into a ParamPack JSON blob
	// suitable for storage in the DB's param_pack column.
	EncodeResult(g Gene, spawn *quant.SpawnPoint) ([]byte, error)
}
