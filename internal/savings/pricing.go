package savings

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
)

// Price is input-token pricing in USD per 1M tokens. Tokens saved are input
// tokens that would have been sent to the model, so we always bill them at
// the input rate.
type Price struct {
	Model        string  `json:"model"`
	USDPerMInput float64 `json:"usd_per_m_input"`
}

// defaultPricing is the built-in table used when no override is configured.
// Input-token list prices in USD per 1M tokens, covering the providers
// gortex's LLM features talk to: Anthropic, OpenAI (also reachable via
// Azure), Google Gemini and DeepSeek — plus Zhipu's GLM line, which
// reaches the ledger as a client model rather than as a gortex provider.
// Bedrock-hosted Anthropic/OpenAI ids resolve through findPrice's
// substring match. Tokens saved are billed at the input rate. Users can
// override the whole table via
// GORTEX_MODEL_PRICING_JSON='[{"model":"...","usd_per_m_input":N},...]'.
var defaultPricing = []Price{
	// Anthropic — claude-opus-4-8 is the headline default (see savings.go).
	// Keep the current generation listed first: findPrice resolves an exact
	// match before any substring match, so a missing row is what turns a live
	// model's cost avoided into a silent $0.00.
	{"claude-fable-5", 10.00},
	{"claude-mythos-5", 10.00},
	{"claude-mythos-preview", 10.00},
	{"claude-opus-5", 5.00},
	{"claude-opus-4-8", 5.00},
	{"claude-opus-4-7", 5.00},
	{"claude-opus-4-6", 5.00},
	// claude-sonnet-5 bills at its $2/M introductory input rate, which
	// Anthropic made permanent on 2026-08-10 (the planned $3/$15 increase
	// was withdrawn), unlike sonnet-4-6 which lists at $3.
	{"claude-sonnet-5", 2.00},
	{"claude-sonnet-4-6", 3.00},
	{"claude-haiku-4-5", 1.00},
	// OpenAI (also reachable via Azure). The gpt-5.6 tiers are the slugs the
	// codex CLI reports, so they show up in savings attribution even though
	// gortex never calls them directly.
	{"gpt-5.6-sol", 5.00},
	{"gpt-5.6-terra", 2.50},
	{"gpt-5.6-luna", 1.00},
	{"gpt-5.5", 5.00},
	{"gpt-5.4", 2.50},
	{"gpt-5.4-mini", 0.75},
	{"gpt-5.4-nano", 0.20},
	{"gpt-4.1", 2.00},
	{"gpt-4.1-mini", 0.40},
	{"gpt-4.1-nano", 0.10},
	{"gpt-4o", 2.50},
	{"gpt-4o-mini", 0.15},
	{"o3", 2.00},
	{"o4-mini", 1.10},
	// Google Gemini — gemini-2.5-pro is gortex's Gemini default.
	{"gemini-3.1-pro", 2.00},
	{"gemini-3.5-flash", 1.50},
	{"gemini-3.1-flash-lite", 0.25},
	{"gemini-2.5-pro", 1.25},
	{"gemini-2.5-flash", 0.30},
	{"gemini-2.5-flash-lite", 0.10},
	// DeepSeek — deepseek-chat / deepseek-reasoner are gortex's configured ids.
	{"deepseek-chat", 0.27},
	{"deepseek-reasoner", 0.42},
	// Zhipu GLM (docs.z.ai list prices). These are client-model slugs — a
	// coding agent pointed at Zhipu's Anthropic-compatible endpoint, or at
	// Ollama Cloud, reports them into savings attribution even though
	// gortex never calls them itself. Every tier gets its own row on
	// purpose: findPrice falls back to a substring match, so omitting
	// glm-4.5-air would silently bill it at the glm-4.5 rate — 3x its
	// real price. The two free Flash tiers carry an explicit 0.00 for the
	// same reason; they surface as "unpriced", which beats overcharging.
	{"glm-5.2", 1.40},
	{"glm-5.1", 1.40},
	{"glm-5-turbo", 1.20},
	{"glm-5", 1.00},
	{"glm-4.7", 0.60},
	{"glm-4.7-flashx", 0.07},
	{"glm-4.7-flash", 0.00},
	{"glm-4.6", 0.60},
	{"glm-4.5", 0.60},
	{"glm-4.5-x", 2.20},
	{"glm-4.5-airx", 1.10},
	{"glm-4.5-air", 0.20},
	{"glm-4.5-flash", 0.00},
}

// Pricing returns the active pricing table — default unless overridden by
// GORTEX_MODEL_PRICING_JSON. Falls back to the default on malformed JSON.
func Pricing() []Price {
	raw := os.Getenv("GORTEX_MODEL_PRICING_JSON")
	if raw == "" {
		return defaultPricing
	}
	var custom []Price
	if err := json.Unmarshal([]byte(raw), &custom); err != nil || len(custom) == 0 {
		return defaultPricing
	}
	return custom
}

// CostAvoided returns the dollar value of tokensSaved evaluated against the
// pricing row whose model matches `name` (case-insensitive, substring).
// Returns 0 when no matching entry exists.
func CostAvoided(tokensSaved int64, name string) float64 {
	if tokensSaved <= 0 {
		return 0
	}
	p := findPrice(name)
	if p == nil {
		return 0
	}
	return float64(tokensSaved) * p.USDPerMInput / 1_000_000.0
}

// ModelRate returns the USD-per-1M-token input rate for the named model
// (case-insensitive, substring — see findPrice), or 0 when no pricing row
// matches. Callers that need a ProviderPricing-shaped rate card (the
// review cost block) use this to derive one from the built-in table.
func ModelRate(name string) float64 {
	p := findPrice(name)
	if p == nil {
		return 0
	}
	return p.USDPerMInput
}

// CostAvoidedAll returns the cost across every entry in the pricing table,
// keyed by model name — useful for the CLI's multi-model summary.
func CostAvoidedAll(tokensSaved int64) map[string]float64 {
	out := make(map[string]float64, len(defaultPricing))
	for _, p := range Pricing() {
		out[p.Model] = float64(tokensSaved) * p.USDPerMInput / 1_000_000.0
	}
	return out
}

// findPrice locates a pricing entry by case-insensitive substring match on
// model name so callers can pass "opus" or "claude-opus-4-8" interchangeably.
func findPrice(name string) *Price {
	if name == "" {
		return nil
	}
	target := strings.ToLower(name)
	prices := Pricing()
	// Exact match first, longest substring second — avoids "claude" matching
	// "claude-opus-4-8" when the user passed "claude-sonnet-4-6".
	for i := range prices {
		if strings.EqualFold(prices[i].Model, name) {
			return &prices[i]
		}
	}
	type hit struct {
		i      int
		length int
	}
	var hits []hit
	for i := range prices {
		m := strings.ToLower(prices[i].Model)
		if strings.Contains(m, target) || strings.Contains(target, m) {
			hits = append(hits, hit{i, len(prices[i].Model)})
		}
	}
	if len(hits) == 0 {
		return nil
	}
	sort.Slice(hits, func(a, b int) bool { return hits[a].length > hits[b].length })
	return &prices[hits[0].i]
}
