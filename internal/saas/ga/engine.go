package ga

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"runtime"
	"sort"
	"sync"

	"gorm.io/gorm"

	"quantsaas/internal/quant"
	"quantsaas/internal/saas/store"
)

// EvolutionEngine orchestrates the GA lifecycle.
// It is fully strategy-agnostic: all strategy-specific logic goes through EvolvableStrategy.
// Docs reference: 進化計算引擎.md §2
type EvolutionEngine struct {
	evolvable EvolvableStrategy
	db        *gorm.DB

	// Hyperparameters with defaults from the docs.
	PopSize                int
	MaxGenerations         int
	EliteCount             int
	MutationProbability    float64
	MutationScale          float64
	MutationProbabilityMax float64
	MutationScaleMax       float64
	MutationRampFactor     float64
	EarlyStopPatience      int
	EarlyStopMinDelta      float64
	TournamentSize         int
}

// EpochConfig carries per-run settings for RunEpoch.
type EpochConfig struct {
	StrategyID     uint   // DB ID of the StrategyTemplate record
	TaskID         uint   // DB ID of the EvolutionTask (for progress updates)
	Symbol         string // e.g. "BTCUSDT"
	Interval       string // e.g. "1d"
	PopSize        int    // overrides engine default when > 0
	MaxGenerations int    // overrides engine default when > 0
	LotStep        float64
	LotMin         float64
	// SpawnPointOverride is the Epoch-level SpawnPoint resolved by the service.
	// The engine uses this directly; it must not be nil.
	SpawnPointOverride *quant.SpawnPoint
	// OnProgress is called after each generation with the current stats.
	OnProgress func(gen int, best float64, mutProb, mutScale float64)
}

// NewEvolutionEngine creates an engine with the default hyperparameters from the docs.
func NewEvolutionEngine(ev EvolvableStrategy, db *gorm.DB) *EvolutionEngine {
	return &EvolutionEngine{
		evolvable:              ev,
		db:                     db,
		PopSize:                300,
		MaxGenerations:         25,
		EliteCount:             8,
		MutationProbability:    0.15,
		MutationScale:          1.0,
		MutationProbabilityMax: 0.55,
		MutationScaleMax:       3.0,
		MutationRampFactor:     1.25,
		EarlyStopPatience:      5,
		EarlyStopMinDelta:      0.001,
		TournamentSize:         3,
	}
}

type individual struct {
	gene    Gene
	fitness float64
}

// RunEpoch executes one full evolution epoch and writes the winning gene to DB
// as a "challenger" GeneRecord.
// Docs reference: 進化計算引擎.md §2
func (e *EvolutionEngine) RunEpoch(ctx context.Context, cfg EpochConfig) (EpochResult, error) {
	if cfg.SpawnPointOverride == nil {
		return EpochResult{}, fmt.Errorf("RunEpoch: SpawnPointOverride must not be nil")
	}

	popSize := cfg.PopSize
	if popSize <= 0 {
		popSize = e.PopSize
	}
	maxGen := cfg.MaxGenerations
	if maxGen <= 0 {
		maxGen = e.MaxGenerations
	}

	// Step 1: Build EvaluablePlan (fetch KLines, build windows, pre-compute DCA baselines).
	plan, err := e.buildPlan(ctx, cfg)
	if err != nil {
		return EpochResult{}, fmt.Errorf("RunEpoch buildPlan: %w", err)
	}
	if len(plan.Windows) == 0 {
		return EpochResult{}, fmt.Errorf("RunEpoch: no crucible windows could be built (insufficient KLine data)")
	}

	rng := rand.New(rand.NewSource(rand.Int63()))

	// Step 2: Initialise population.
	pop := e.initPopulation(cfg.StrategyID, popSize, rng)

	// Step 3: Evaluate initial population.
	scores := e.evaluatePopulation(ctx, pop, plan)
	inds := buildIndividuals(pop, scores)
	sortByFitness(inds)

	bestEver := inds[0].fitness
	patienceCount := 0
	mutProb := e.MutationProbability
	mutScale := e.MutationScale
	genDone := 0

	// Step 4: Main evolution loop.
	for gen := 0; gen < maxGen; gen++ {
		select {
		case <-ctx.Done():
			goto done
		default:
		}

		// Convergence detection.
		if inds[0].fitness-bestEver < e.EarlyStopMinDelta {
			patienceCount++
		} else {
			bestEver = inds[0].fitness
			patienceCount = 0
		}

		// Mutation ramp: gradually increase mutation when stuck.
		if patienceCount >= e.EarlyStopPatience {
			mutProb = min(mutProb*e.MutationRampFactor, e.MutationProbabilityMax)
			mutScale = min(mutScale*e.MutationRampFactor, e.MutationScaleMax)
		}

		if cfg.OnProgress != nil {
			cfg.OnProgress(gen, inds[0].fitness, mutProb, mutScale)
		}

		// Early stop: both ramps are maxed and there is still no improvement.
		if patienceCount >= e.EarlyStopPatience &&
			mutProb >= e.MutationProbabilityMax &&
			mutScale >= e.MutationScaleMax {
			genDone = gen + 1
			goto done
		}

		// Produce next generation.
		nextPop := make([]Gene, 0, popSize)

		// Elite preservation: top EliteCount individuals pass unchanged.
		eliteN := e.EliteCount
		if eliteN > len(inds) {
			eliteN = len(inds)
		}
		for i := range eliteN {
			nextPop = append(nextPop, inds[i].gene)
		}

		// Fill remainder via tournament selection + crossover + mutation.
		for len(nextPop) < popSize {
			p1 := tournamentSelect(inds, e.TournamentSize, rng)
			p2 := tournamentSelect(inds, e.TournamentSize, rng)
			child := e.evolvable.Crossover(p1, p2, rng)
			child = e.evolvable.Mutate(child, mutProb, mutScale, rng)
			nextPop = append(nextPop, child)
		}

		scores = e.evaluatePopulation(ctx, nextPop, plan)
		inds = buildIndividuals(nextPop, scores)
		sortByFitness(inds)
		genDone = gen + 1
	}
	if genDone == 0 {
		genDone = maxGen
	}

done:
	bestGene := inds[0].gene

	// Re-evaluate the best gene to get per-window detail for storage.
	_, windowResults := e.evolvable.Evaluate(ctx, bestGene, plan)

	windowScoresJSON, _ := json.Marshal(windowResultsToMap(windowResults))

	paramJSON, err := e.evolvable.EncodeResult(bestGene, plan.Spawn)
	if err != nil {
		return EpochResult{}, fmt.Errorf("RunEpoch EncodeResult: %w", err)
	}

	// Compute overall MaxDD for the record.
	maxDD := 0.0
	for _, r := range windowResults {
		if r.MaxDD > maxDD {
			maxDD = r.MaxDD
		}
	}

	rec := store.GeneRecord{
		StrategyID:   cfg.StrategyID,
		TaskID:       cfg.TaskID,
		Role:         "challenger",
		ParamPack:    string(paramJSON),
		ScoreTotal:   inds[0].fitness,
		MaxDrawdown:  maxDD,
		WindowScores: string(windowScoresJSON),
	}
	if err := e.db.WithContext(ctx).Create(&rec).Error; err != nil {
		return EpochResult{}, fmt.Errorf("RunEpoch write challenger: %w", err)
	}

	return EpochResult{
		BestScore:    inds[0].fitness,
		Generations:  genDone,
		ChallengerID: rec.ID,
	}, nil
}

// buildPlan fetches KLines and constructs the immutable EvaluablePlan.
func (e *EvolutionEngine) buildPlan(ctx context.Context, cfg EpochConfig) (EvaluablePlan, error) {
	var klines []store.KLine
	if err := e.db.WithContext(ctx).
		Where("symbol = ? AND interval = ?", cfg.Symbol, cfg.Interval).
		Order("open_time asc").
		Find(&klines).Error; err != nil {
		return EvaluablePlan{}, fmt.Errorf("buildPlan query KLines: %w", err)
	}

	bars := klinesToBars(klines)
	windows := quant.BuildCrucibleWindows(bars, quant.DefaultWarmupDays)

	// Pre-compute DCA baselines once for all windows.
	baselines := make([]DCABaseline, len(windows))
	for i, w := range windows {
		evalBars := barsFrom(w.Bars, w.EvalStartMs)
		res := quant.SimulateGhostDCA(quant.GhostDCAConfig{
			InitialCapital: cfg.SpawnPointOverride.InitialCapital,
			MonthlyInject:  cfg.SpawnPointOverride.MonthlyInject,
		}, evalBars)
		baselines[i] = DCABaseline{
			FinalEquity:   res.FinalEquity,
			TotalInjected: res.TotalInjected,
			MaxDrawdown:   res.MaxDrawdown,
			ROI:           res.ROI,
		}
	}

	return EvaluablePlan{
		Pair:         cfg.Symbol,
		TemplateName: e.evolvable.StrategyID(),
		Spawn:        cfg.SpawnPointOverride,
		LotStep:      cfg.LotStep,
		LotMin:       cfg.LotMin,
		Windows:      windows,
		DCABaselines: baselines,
	}, nil
}

// initPopulation builds the initial population following the docs' 10/40/50 rule.
// Index 0 is always the current seed champion (or the default seed).
// Docs reference: 進化計算引擎.md §2.1
func (e *EvolutionEngine) initPopulation(strategyID uint, popSize int, rng *rand.Rand) []Gene {
	var elites []store.GeneRecord
	e.db.Where("strategy_id = ? AND role IN ?", strategyID, []string{"champion", "challenger"}).
		Order("score_total desc").
		Limit(popSize).
		Find(&elites)

	pop := make([]Gene, 0, popSize)

	// Index 0: current seed champion (raw, no mutation).
	if len(elites) > 0 {
		pop = append(pop, e.evolvable.DecodeElite([]byte(elites[0].ParamPack)))
	} else {
		pop = append(pop, e.evolvable.DecodeElite(nil)) // returns DefaultSeedChromosome
	}

	if len(elites) > 0 && popSize > 1 {
		remaining := popSize - 1 // slots after index 0
		eliteCount := max(1, remaining*10/100)
		mutCount := remaining * 40 / 100
		// randomCount fills the rest

		// ~10% elite copies.
		for i := 0; i < eliteCount && len(pop) < popSize; i++ {
			pop = append(pop, e.evolvable.DecodeElite([]byte(elites[i%len(elites)].ParamPack)))
		}
		// ~40% elite + strong mutation (fixed prob=0.15, scale=1.5 per docs §2.1).
		for i := 0; i < mutCount && len(pop) < popSize; i++ {
			base := e.evolvable.DecodeElite([]byte(elites[i%len(elites)].ParamPack))
			pop = append(pop, e.evolvable.Mutate(base, 0.15, 1.5, rng))
		}
	}

	// ~50% (or all remaining when no elites) fully random.
	for len(pop) < popSize {
		pop = append(pop, e.evolvable.Sample(rng))
	}

	return pop
}

// evaluatePopulation concurrently evaluates the entire population.
// A sync.Map is used to cache fitness by fingerprint, skipping duplicate evaluations.
// Docs reference: 進化計算引擎.md §2.2 and §2.8
func (e *EvolutionEngine) evaluatePopulation(ctx context.Context, pop []Gene, plan EvaluablePlan) []float64 {
	n := len(pop)
	scores := make([]float64, n)

	workers := runtime.NumCPU()
	if workers > n {
		workers = n
	}

	type job struct {
		idx  int
		gene Gene
	}

	jobs := make(chan job, n)
	for i, g := range pop {
		jobs <- job{i, g}
	}
	close(jobs)

	var fpCache sync.Map // uint64 → float64
	var wg sync.WaitGroup
	ev := e.evolvable // capture to avoid data race on e

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				fp := ev.Fingerprint(j.gene)
				if cached, ok := fpCache.Load(fp); ok {
					scores[j.idx] = cached.(float64)
					continue
				}
				score, _ := ev.Evaluate(ctx, j.gene, plan)
				fpCache.Store(fp, score)
				scores[j.idx] = score
			}
		}()
	}
	wg.Wait()
	return scores
}

// tournamentSelect picks TournamentSize distinct individuals at random and
// returns the gene of the fittest one.
// Docs reference: 進化計算引擎.md §2.3
func tournamentSelect(inds []individual, size int, rng *rand.Rand) Gene {
	n := len(inds)
	best := inds[rng.Intn(n)]
	for range size - 1 {
		candidate := inds[rng.Intn(n)]
		if candidate.fitness > best.fitness {
			best = candidate
		}
	}
	return best.gene
}

// buildIndividuals zips a population slice with its fitness scores.
func buildIndividuals(pop []Gene, scores []float64) []individual {
	inds := make([]individual, len(pop))
	for i := range pop {
		inds[i] = individual{gene: pop[i], fitness: scores[i]}
	}
	return inds
}

// sortByFitness sorts individuals descending by fitness (best first).
func sortByFitness(inds []individual) {
	sort.Slice(inds, func(i, j int) bool {
		return inds[i].fitness > inds[j].fitness
	})
}

// klinesToBars converts store.KLine DB records to quant.Bar values.
func klinesToBars(klines []store.KLine) []quant.Bar {
	bars := make([]quant.Bar, len(klines))
	for i, k := range klines {
		bars[i] = quant.Bar{
			OpenTime: k.OpenTime.UnixMilli(),
			Open:     k.Open,
			High:     k.High,
			Low:      k.Low,
			Close:    k.Close,
			Volume:   k.Volume,
		}
	}
	return bars
}

// barsFrom returns a sub-slice of bars with OpenTime >= fromMs.
func barsFrom(bars []quant.Bar, fromMs int64) []quant.Bar {
	for i, b := range bars {
		if b.OpenTime >= fromMs {
			return bars[i:]
		}
	}
	return nil
}

// windowResultsToMap converts a CrucibleResult slice to a label→score map for JSON storage.
func windowResultsToMap(results []quant.CrucibleResult) map[string]float64 {
	m := make(map[string]float64, len(results))
	for _, r := range results {
		m[r.Window] = r.Score
	}
	return m
}
