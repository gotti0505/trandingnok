package sigmoid_dca

import (
	"encoding/json"

	"quantsaas/internal/quant"
)

// paramPack mirrors the JSON stored in the DB param_pack column.
// Docs reference: 策略數學引擎.md §7.3
type paramPack struct {
	SpawnPoint quant.SpawnPoint `json:"spawn_point"`
	Config     quant.Chromosome `json:"sigmoid_dca_config"`
}

// ParseParamPack decodes the raw JSON param pack into a Chromosome and SpawnPoint.
// On any decode failure it returns DefaultSeedChromosome + zero SpawnPoint so the
// system can still tick rather than halt.
func ParseParamPack(raw string) (quant.Chromosome, quant.SpawnPoint) {
	var pp paramPack
	if err := json.Unmarshal([]byte(raw), &pp); err != nil {
		return quant.DefaultSeedChromosome, quant.SpawnPoint{}
	}
	quant.ClampChromosome(&pp.Config)
	return pp.Config, pp.SpawnPoint
}
