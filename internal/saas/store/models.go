package store

import (
	"time"

	"gorm.io/gorm"
)

// User holds account credentials and subscription plan metadata.
type User struct {
	gorm.Model
	Email        string `gorm:"uniqueIndex;not null"`
	PasswordHash string `gorm:"not null"`
	Plan         string `gorm:"not null;default:'free'"` // free / pro / enterprise
	MaxInstances int    `gorm:"not null;default:1"`
	IsActive     bool   `gorm:"not null;default:true"`
}

// StrategyTemplate is the immutable "blueprint" for a strategy.
// Manifest is a JSON blob carrying default parameter schema.
type StrategyTemplate struct {
	gorm.Model
	Name     string `gorm:"not null"`
	Version  string `gorm:"not null"`
	IsSpot   bool   `gorm:"not null;default:true"`
	Manifest string `gorm:"type:jsonb;not null;default:'{}'"`
}

// StrategyInstance binds a template to a user + symbol + fund allocation.
// Status transitions: STOPPED → RUNNING → STOPPED | ERROR; any → DELETED
type StrategyInstance struct {
	gorm.Model
	UserID     uint   `gorm:"not null;index"`
	TemplateID uint   `gorm:"not null;index"`
	Symbol     string `gorm:"not null"`
	Interval   string `gorm:"not null;default:'1min'"` // candle aggregation period for Tick
	Status     string `gorm:"not null;default:'STOPPED'"` // RUNNING / STOPPED / ERROR / DELETED
	ParamPack  string `gorm:"type:jsonb;not null;default:'{}'"`

	User     User             `gorm:"foreignKey:UserID"`
	Template StrategyTemplate `gorm:"foreignKey:TemplateID"`
}

// PortfolioState is the authoritative account snapshot for one instance.
// Updated every time an Agent uploads a DeltaReport.
type PortfolioState struct {
	gorm.Model
	InstanceID           uint      `gorm:"not null;uniqueIndex"`
	USDTBalance          float64   `gorm:"not null;default:0"`
	DeadBTC              float64   `gorm:"not null;default:0"` // macro floor — never sold
	FloatBTC             float64   `gorm:"not null;default:0"` // micro floating position
	ColdSealedBTC        float64   `gorm:"not null;default:0"` // sealed from new buys
	TotalEquity          float64   `gorm:"not null;default:0"`
	LastProcessedBarTime time.Time
}

// RuntimeState persists the opaque JSON blob emitted by Step().
// One row per instance; upserted after every successful Tick.
type RuntimeState struct {
	gorm.Model
	InstanceID uint   `gorm:"not null;uniqueIndex"`
	Payload    string `gorm:"type:jsonb;not null;default:'{}'"`
}

// SpotLot records a single lot position with its semantic type.
// LotType mirrors the倉位三態語義 defined in the topology doc.
type SpotLot struct {
	gorm.Model
	InstanceID   uint    `gorm:"not null;index"`
	LotType      string  `gorm:"not null"` // DEAD_STACK / FLOATING / COLD_SEALED
	Amount       float64 `gorm:"not null"`
	CostPrice    float64 `gorm:"not null"`
	IsColdSealed bool    `gorm:"not null;default:false"`
}

// TradeRecord is the settled trade ledger entry created after Agent confirms fill.
type TradeRecord struct {
	gorm.Model
	InstanceID    uint    `gorm:"not null;index"`
	ClientOrderID string  `gorm:"uniqueIndex;not null"` // format: inst{id}-{type}-{ts}
	Action        string  `gorm:"not null"`             // BUY / SELL
	Engine        string  `gorm:"not null"`             // MACRO / MICRO
	Symbol        string  `gorm:"not null"`
	FilledQty     float64 `gorm:"not null;default:0"`
	FilledPrice   float64 `gorm:"not null;default:0"`
	Fee           float64 `gorm:"not null;default:0"`
}

// SpotExecution tracks the lifecycle of a single order dispatched to an Agent.
// State machine: pending → filled | failed
type SpotExecution struct {
	gorm.Model
	InstanceID    uint   `gorm:"not null;index"`
	ClientOrderID string `gorm:"uniqueIndex;not null"`
	Status        string `gorm:"not null;default:'pending'"` // pending / filled / failed
	RawPayload    string `gorm:"type:jsonb;not null;default:'{}'"`
}

// AuditLog is an append-only event journal.
type AuditLog struct {
	gorm.Model
	InstanceID uint   `gorm:"index"`
	EventType  string `gorm:"not null"`
	Payload    string `gorm:"type:jsonb;not null;default:'{}'"`
}

// GeneRecord stores one chromosome produced by the evolution engine.
// Role lifecycle: challenger → champion (after human approval) → retired
type GeneRecord struct {
	gorm.Model
	StrategyID   uint    `gorm:"not null;index"`
	TaskID       uint    `gorm:"index"` // FK to EvolutionTask that produced this record
	Role         string  `gorm:"not null"` // challenger / champion / retired
	ParamPack    string  `gorm:"type:jsonb;not null"`
	ScoreTotal   float64 `gorm:"not null;default:0"`
	MaxDrawdown  float64 `gorm:"not null;default:0"`
	WindowScores string  `gorm:"type:jsonb;not null;default:'{}'"`
}

// EvolutionTask represents one GA run.
// Status: pending → running → done | failed
type EvolutionTask struct {
	gorm.Model
	StrategyID        uint    `gorm:"not null;index"`
	Status            string  `gorm:"not null;default:'pending'"` // pending / running / done / failed
	Progress          float64 `gorm:"not null;default:0"`
	CurrentGeneration int     `gorm:"not null;default:0"`
	BestScore         float64 `gorm:"not null;default:0"`
	ErrorMsg          string  `gorm:"type:text;not null;default:''"`
	Config            string  `gorm:"type:jsonb;not null;default:'{}'"`
}

// KLine stores historical OHLCV bars.
// The compound unique index prevents duplicate ingestion.
type KLine struct {
	ID       uint      `gorm:"primaryKey;autoIncrement"`
	Symbol   string    `gorm:"not null;index:idx_kline_uniq,unique"`
	Interval string    `gorm:"not null;index:idx_kline_uniq,unique"`
	OpenTime time.Time `gorm:"not null;index:idx_kline_uniq,unique"`
	Open     float64   `gorm:"not null"`
	High     float64   `gorm:"not null"`
	Low      float64   `gorm:"not null"`
	Close    float64   `gorm:"not null"`
	Volume   float64   `gorm:"not null"`
}
