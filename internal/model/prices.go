package model

import (
	"sort"
	"strings"

	"github.com/blakemcbride/ClaudeCodeTaskMonitor/internal/transcript"
)

// ModelInfo is the price and context window of one model. Prices are US
// dollars per million tokens.
type ModelInfo struct {
	Input        float64 `json:"input"`
	CacheWrite5m float64 `json:"cacheWrite5m"`
	CacheWrite1h float64 `json:"cacheWrite1h"`
	CacheRead    float64 `json:"cacheRead"`
	Output       float64 `json:"output"`
	Context      int64   `json:"context"` // tokens
}

// DefaultModels is keyed by a prefix of the model id; the longest matching
// prefix wins. Figures are from Anthropic's published pricing as of November
// 2026 and will go stale: override them in the config file.
//
// Not modelled: Claude Haiku 5.5 costs more on requests over 100,000 prompt
// tokens, so its estimate here is a lower bound.
var DefaultModels = map[string]ModelInfo{
	"claude-fable-5-2":  {8, 10, 16, 0.20, 40, 1_000_000},
	"claude-mythos-5-2": {8, 10, 16, 0.20, 40, 1_000_000},
	"claude-opus-5-5":   {4, 5, 8, 0.20, 20, 1_000_000},
	"claude-sonnet-5-5": {2, 2.50, 4, 0.10, 10, 1_000_000},
	"claude-haiku-5-5":  {0.10, 0.125, 0.20, 0.01, 0.50, 1_000_000},
	"claude-haiku-4-5":  {1, 1.25, 2, 0.10, 5, 200_000},
}

// Lookup finds the entry whose key is the longest prefix of the model id.
func Lookup(models map[string]ModelInfo, id string) (ModelInfo, bool) {
	keys := make([]string, 0, len(models))
	for k := range models {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, k := range keys {
		if strings.HasPrefix(id, k) {
			return models[k], true
		}
	}
	return ModelInfo{}, false
}

// Cost prices a token count.
func (m ModelInfo) Cost(u transcript.Usage) float64 {
	return (float64(u.In)*m.Input + float64(u.CacheWrite5m)*m.CacheWrite5m +
		float64(u.CacheWrite1h)*m.CacheWrite1h + float64(u.CacheRead)*m.CacheRead +
		float64(u.Out)*m.Output) / 1e6
}
