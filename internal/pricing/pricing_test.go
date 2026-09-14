package pricing

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

// writeTable puts a price table in a temp dir and loads it.
func writeTable(t *testing.T, body string) *Table {
	t.Helper()

	path := filepath.Join(t.TempDir(), "pricing.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write table: %v", err)
	}
	table, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return table
}

const testTable = `
version: 1
providers:
  openai:
    gpt-5:
      input: 1.25
      cached_input: 0.125
      output: 10.00
    gpt-5-codex:
      input: 2.00
      cached_input: 0.20
      output: 20.00
    no-cache-rate:
      input: 4.00
      output: 8.00
  anthropic:
    claude-sonnet-4-6:
      input: 3.00
      cached_input: 0.30
      output: 15.00
`

// TestCostCacheAccounting is the case worth guarding: the two providers count
// cache hits differently, and treating them the same silently misprices every
// cached call — which for a coding agent is nearly all of them.
func TestCostCacheAccounting(t *testing.T) {
	table := writeTable(t, testTable)

	tests := []struct {
		name                       string
		provider, model            string
		input, cachedInput, output int64
		want                       float64
	}{
		{
			// OpenAI: prompt_tokens INCLUDES the cached ones, so of 1M input
			// tokens only 200k are billed at the full rate.
			name:     "openai subtracts cached tokens from input",
			provider: "openai", model: "gpt-5",
			input: 1_000_000, cachedInput: 800_000, output: 0,
			want: 0.2*1.25 + 0.8*0.125,
		},
		{
			// Anthropic: input_tokens EXCLUDES cache reads, so all 1M are
			// billed at the full rate on top of the cached ones.
			name:     "anthropic adds cached tokens to input",
			provider: "anthropic", model: "claude-sonnet-4-6",
			input: 1_000_000, cachedInput: 800_000, output: 0,
			want: 1.0*3.00 + 0.8*0.30,
		},
		{
			name:     "output tokens are billed at the output rate",
			provider: "openai", model: "gpt-5",
			input: 0, cachedInput: 0, output: 500_000,
			want: 0.5 * 10.00,
		},
		{
			name:     "a missing cached rate falls back to the input rate",
			provider: "openai", model: "no-cache-rate",
			input: 1_000_000, cachedInput: 500_000, output: 0,
			want: 0.5*4.00 + 0.5*4.00,
		},
		{
			// A malformed response claiming more cached than total input must
			// not produce a negative charge.
			name:     "nonsensical cache counts never yield a credit",
			provider: "openai", model: "gpt-5",
			input: 100, cachedInput: 5_000, output: 0,
			want: 5000 * 0.125 / 1_000_000,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := table.Cost(tc.provider, tc.model, tc.input, tc.cachedInput, tc.output)
			if !ok {
				t.Fatalf("Cost reported the model as unknown")
			}
			if math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("Cost = %v, want %v", got, tc.want)
			}
		})
	}
}

// Providers return dated snapshot names. Matching the longest prefix keeps the
// table maintainable without letting a shorter entry shadow a longer one.
func TestLookupPrefixMatching(t *testing.T) {
	table := writeTable(t, testTable)

	tests := []struct {
		model     string
		wantInput float64
	}{
		{"gpt-5", 1.25},
		{"gpt-5-2026-01-15", 1.25},
		{"gpt-5-codex", 2.00},
		{"gpt-5-codex-2026-03-01", 2.00}, // must not fall back to gpt-5
		{"GPT-5-CODEX", 2.00},            // model names are case-insensitive
	}

	for _, tc := range tests {
		t.Run(tc.model, func(t *testing.T) {
			got, ok := table.lookup("openai", tc.model)
			if !ok {
				t.Fatalf("lookup(%q) found nothing", tc.model)
			}
			if got.Input != tc.wantInput {
				t.Errorf("lookup(%q).Input = %v, want %v", tc.model, got.Input, tc.wantInput)
			}
		})
	}
}

func TestCostUnknownModel(t *testing.T) {
	table := writeTable(t, testTable)

	for _, tc := range []struct{ provider, model string }{
		{"openai", "some-unreleased-model"},
		{"cohere", "gpt-5"}, // right model name, wrong provider
		{"openai", ""},
	} {
		if _, ok := table.Cost(tc.provider, tc.model, 100, 0, 100); ok {
			t.Errorf("Cost(%q, %q) reported a price, want unknown", tc.provider, tc.model)
		}
	}
}

func TestShouldWarnOnlyOncePerModel(t *testing.T) {
	table := writeTable(t, testTable)

	if !table.ShouldWarn("openai", "mystery") {
		t.Error("first ShouldWarn = false, want true")
	}
	if table.ShouldWarn("openai", "mystery") {
		t.Error("second ShouldWarn = true, want false")
	}
	if !table.ShouldWarn("openai", "other-mystery") {
		t.Error("ShouldWarn for a different model = false, want true")
	}
}

func TestLoadRejectsBadTables(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"missing version", "providers: {}"},
		{"future version", "version: 2\nproviders: {}"},
		{"not yaml", "version: 1\n\tproviders: ["},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "pricing.yaml")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			if _, err := Load(path); err == nil {
				t.Error("Load accepted the table, want an error")
			}
		})
	}

	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Error("Load accepted a missing file, want an error")
	}
}

// The shipped table must stay loadable and internally sane; it is the file
// most likely to be hand-edited.
func TestShippedTableLoads(t *testing.T) {
	table, err := Load(filepath.Join("..", "..", "configs", "pricing.yaml"))
	if err != nil {
		t.Fatalf("load shipped table: %v", err)
	}

	for provider, models := range table.providers {
		if len(models) == 0 {
			t.Errorf("provider %q has no models", provider)
		}
		for model, price := range models {
			if price.Input <= 0 || price.Output <= 0 {
				t.Errorf("%s/%s: input and output rates must be positive, got %+v", provider, model, price)
			}
			if price.CachedInput > price.Input {
				t.Errorf("%s/%s: cached rate %v exceeds the input rate %v", provider, model, price.CachedInput, price.Input)
			}
		}
	}
}
