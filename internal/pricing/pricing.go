// Package pricing turns token counts into a dollar figure using a local,
// user-editable price table.
//
// Prices are deliberately not compiled in. They change often, they differ per
// contract, and a table in the repository would be wrong for most people
// running llmtap.
package pricing

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// perMillion is the unit every price in the table is expressed in.
const perMillion = 1_000_000.0

// ModelPrice is the cost of a model in USD per million tokens.
type ModelPrice struct {
	Input  float64 `yaml:"input"`
	Output float64 `yaml:"output"`

	// CachedInput is the discounted rate for a cache hit. Zero means cache
	// hits are billed at the normal input rate.
	CachedInput float64 `yaml:"cached_input"`
}

// file is the on-disk shape of the price table.
type file struct {
	Version   int                              `yaml:"version"`
	Updated   string                           `yaml:"updated"`
	Providers map[string]map[string]ModelPrice `yaml:"providers"`
}

// Table answers cost questions. It is read-only after Load and safe for
// concurrent use.
type Table struct {
	providers map[string]map[string]ModelPrice

	// warned remembers which unknown models have already been reported, so a
	// missing price produces one log line rather than one per request.
	warned sync.Map
}

// Load reads a price table from a YAML file.
func Load(path string) (*Table, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the path is operator-supplied config
	if err != nil {
		return nil, fmt.Errorf("read price table: %w", err)
	}

	var parsed file
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("parse price table %s: %w", path, err)
	}
	if parsed.Version != 1 {
		return nil, fmt.Errorf("price table %s: unsupported version %d, want 1", path, parsed.Version)
	}

	// Normalize keys once so lookups do not have to.
	providers := make(map[string]map[string]ModelPrice, len(parsed.Providers))
	for provider, models := range parsed.Providers {
		normalized := make(map[string]ModelPrice, len(models))
		for model, price := range models {
			normalized[strings.ToLower(model)] = price
		}
		providers[strings.ToLower(provider)] = normalized
	}

	return &Table{providers: providers}, nil
}

// Cost implements telemetry.Pricer. It reports false for a model the table
// does not cover, which leaves the cost metric unreported rather than wrong.
func (t *Table) Cost(provider, model string, inputTokens, cachedInputTokens, outputTokens int64) (float64, bool) {
	price, ok := t.lookup(provider, model)
	if !ok {
		return 0, false
	}

	cachedRate := price.CachedInput
	if cachedRate == 0 {
		cachedRate = price.Input
	}

	// The two providers count cache hits differently, and getting this wrong
	// silently misprices every cached call:
	//
	//   OpenAI    prompt_tokens already INCLUDES cached_tokens, so the
	//             full-rate portion is the difference.
	//   Anthropic input_tokens EXCLUDES cache_read_input_tokens, so the two
	//             are simply added.
	fullRateInput := inputTokens
	if strings.EqualFold(provider, "openai") {
		fullRateInput = inputTokens - cachedInputTokens
		if fullRateInput < 0 {
			fullRateInput = 0 // defensive: never let a bad response produce a credit
		}
	}

	usd := (float64(fullRateInput)*price.Input +
		float64(cachedInputTokens)*cachedRate +
		float64(outputTokens)*price.Output) / perMillion

	return usd, true
}

// lookup resolves a model name to a price, falling back to the longest
// matching prefix. Providers return dated snapshot names such as
// "gpt-4.1-2025-04-14", and nobody wants to edit the table on every release.
func (t *Table) lookup(provider, model string) (ModelPrice, bool) {
	models, ok := t.providers[strings.ToLower(provider)]
	if !ok {
		return ModelPrice{}, false
	}

	name := strings.ToLower(model)
	if price, ok := models[name]; ok {
		return price, true
	}

	var (
		best    ModelPrice
		bestLen int
	)
	for candidate, price := range models {
		if len(candidate) > bestLen && strings.HasPrefix(name, candidate) {
			best, bestLen = price, len(candidate)
		}
	}
	return best, bestLen > 0
}

// ShouldWarn reports whether an unknown model has not been reported yet. It
// returns true exactly once per provider and model.
func (t *Table) ShouldWarn(provider, model string) bool {
	_, seen := t.warned.LoadOrStore(provider+"/"+model, struct{}{})
	return !seen
}
