# QuantSaaS — Agent Instructions

This file is automatically loaded by AI coding agents (GitHub Copilot, Gemini Code Assist, etc.).

## Single Source of Truth
All features are implemented **only** based on the three documents in `docs/`:
- `docs/系統總體拓撲結構.md` — topology, modules, lifecycle
- `docs/策略數學引擎.md` — Step() contract, Sigmoid formulas, macro/micro engine
- `docs/進化計算引擎.md` — GA algorithm, chromosome, backtest fitness

Features not defined in these documents must NOT be implemented.

## Non-Negotiable Rules
1. **Strategy isomorphism**: backtest and live trading MUST call the same `Step()` implementation. No `if isBacktest` branches.
2. **Pure function**: `Step()` must have zero side effects — no network, no DB, no file I/O.
3. **API Key isolation**: exchange credentials exist ONLY in `config.agent.yaml`. Never in SaaS code or DB.
4. **GORM Code-First**: schema source of truth is Go structs. Only `AutoMigrate`. Never write SQL migration files.
5. **Dimensionless math**: use log-returns or ratios. Never compare absolute prices across assets.

## Directory Contract
```
cmd/saas/             SaaS cloud entry point
cmd/agent/            LocalAgent entry point
internal/saas/        SaaS business logic (no strategy code)
internal/agent/       Agent logic (no strategy code)
internal/strategy/    Strategy interface (Step signature, I/O structs)
internal/strategies/  Concrete strategy packages (pure functions only)
internal/quant/       Quantitative math library (pure functions)
internal/adapters/backtest/  Backtest adapter
```
