package docker

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/registry"
)

// picoclaw resolves a provider name to an endpoint itself, so its configuration
// never carried one. Every agent migrated from it arrives with a model that names
// a provider, names no endpoint, and fails on its first turn with
// `unsupported protocol scheme ""`.
func TestEveryCatalogProviderHasAnEndpoint(t *testing.T) {
	entries, err := SuggestionCatalog()
	if err != nil {
		t.Fatalf("SuggestionCatalog: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the embedded catalog is empty")
	}

	var missing []string
	for _, e := range entries {
		name := strings.ToLower(strings.TrimSpace(e.Provider))
		if name == "" || e.APIBase == "" || templateProviders[name] {
			continue
		}
		if ProviderEndpoint(name) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("providers the catalog defines but the table does not resolve: %v", missing)
	}
}

// The endpoints are the ones picoclaw itself posts to. Spot-checked against the
// providers this deployment actually uses; the rest come from the same catalog
// rows the admin form has always shown.
func TestTheEndpointsAreTheOnesPicoclawUses(t *testing.T) {
	for provider, want := range map[string]string{
		"deepseek":   "https://api.deepseek.com/v1",
		"openai":     "https://api.openai.com/v1",
		"anthropic":  "https://api.anthropic.com/v1",
		"openrouter": "https://openrouter.ai/api/v1",
		"zhipu":      "https://open.bigmodel.cn/api/paas/v4",
		"qwen":       "https://dashscope.aliyuncs.com/compatible-mode/v1",
	} {
		if got := ProviderEndpoint(provider); got != want {
			t.Errorf("ProviderEndpoint(%q) = %q, want %q", provider, got, want)
		}
	}
}

// A provider name comes out of a file an operator typed.
func TestProviderLookupIgnoresCaseAndSpace(t *testing.T) {
	want := ProviderEndpoint("deepseek")
	for _, in := range []string{"DeepSeek", "  deepseek  ", "DEEPSEEK"} {
		if got := ProviderEndpoint(in); got != want {
			t.Errorf("ProviderEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

// Azure's catalog api_base is a SHAPE, not an address: `your-resource` is a
// fill-in-the-blank the admin form shows. Using it as a fallback would turn a
// missing endpoint into a DNS failure against a hostname that never existed,
// which is a worse error than the honest refusal.
func TestATemplateEndpointIsNotOfferedAsADefault(t *testing.T) {
	if got := ProviderEndpoint("azure"); got != "" {
		t.Fatalf("ProviderEndpoint(azure) = %q, want the refusal", got)
	}
	// And the catalog still carries it, because the FORM needs the shape.
	entries, _ := SuggestionCatalog()
	var found bool
	for _, e := range entries {
		if strings.EqualFold(e.Provider, "azure") && e.APIBase != "" {
			found = true
		}
	}
	if !found {
		t.Error("azure lost its catalog entry; the admin form needs the shape it shows")
	}
}

// An unknown provider is "" rather than a guess. A wrong endpoint fails as someone
// else's 404, which is the hardest kind of failure to attribute.
func TestAnUnknownProviderResolvesToNothing(t *testing.T) {
	if got := ProviderEndpoint("not-a-provider"); got != "" {
		t.Errorf("ProviderEndpoint = %q, want empty", got)
	}
	if got := ProviderEndpoint(""); got != "" {
		t.Errorf("ProviderEndpoint(\"\") = %q, want empty", got)
	}
}

// The table is DERIVED from the catalog, not written out again beside it. This is
// the assertion that keeps that true: a provider added to the catalog is resolvable
// with no second edit, and one removed stops resolving.
func TestTheTableIsDerivedFromTheCatalogItself(t *testing.T) {
	raw, err := catalogFS.ReadFile("model-catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	var entries []CatalogEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}
	providers := map[string]bool{}
	for _, e := range entries {
		if e.APIBase != "" && !templateProviders[strings.ToLower(e.Provider)] {
			providers[strings.ToLower(e.Provider)] = true
		}
	}
	// Every provider the file defines, and nothing this table invented.
	for p := range endpointsForTest() {
		if !providers[p] {
			t.Errorf("the table resolves %q, which the catalog does not define", p)
		}
	}
}

// endpointsForTest forces the lazy build and returns the table.
func endpointsForTest() map[string]string {
	ProviderEndpoint("deepseek")
	return endpoints
}

// --- the resolution order -------------------------------------------------

func modelWith(provider, apiBase string) registry.Model {
	return registry.Model{ModelName: "m", Provider: provider, Model: "m", APIBase: apiBase}
}

func agentWith(baseURL string) config.Agent {
	return config.Agent{
		Key:   "alpha",
		Model: &config.ModelConfig{Provider: "deepseek", Name: "deepseek-chat", BaseURL: baseURL},
	}
}

// THE MIGRATION CASE. picoclaw's configuration answers "which provider" and never
// had to answer "which address" -- so an agent moved from it has a model with a
// provider, no endpoint, and no baseUrl anybody thought to add.
func TestAProviderAloneIsEnough(t *testing.T) {
	res := registry.Resolution{Primary: modelWith("deepseek", "")}
	if err := resolveGanglionEndpoints(&res, config.Agent{Key: "alpha"}, "alpha"); err != nil {
		t.Fatalf("resolveGanglionEndpoints: %v", err)
	}
	if res.Primary.APIBase != "https://api.deepseek.com/v1" {
		t.Fatalf("api_base = %q", res.Primary.APIBase)
	}
}

// A CUSTOM MODEL IS FINAL. It is custom precisely because its endpoint is not its
// provider's default; overwriting it would silently point an admin's own
// deployment at somebody else's API.
func TestTheInventorysOwnEndpointIsNeverOverwritten(t *testing.T) {
	const custom = "https://llm.internal.example/v1"
	res := registry.Resolution{Primary: modelWith("deepseek", custom)}
	// Both of the lower sources say something else, and neither may win.
	if err := resolveGanglionEndpoints(&res, agentWith("https://agent.example/v1"), "alpha"); err != nil {
		t.Fatal(err)
	}
	if res.Primary.APIBase != custom {
		t.Fatalf("a custom endpoint was replaced by %q", res.Primary.APIBase)
	}
}

// The agent's own baseUrl outranks the provider default: an operator who wrote one
// meant it, most likely because they run a proxy or a regional endpoint.
func TestTheAgentsBaseUrlOutranksTheProviderDefault(t *testing.T) {
	const mine = "https://my-gateway.example/v1"
	res := registry.Resolution{Primary: modelWith("deepseek", "")}
	if err := resolveGanglionEndpoints(&res, agentWith(mine), "alpha"); err != nil {
		t.Fatal(err)
	}
	if res.Primary.APIBase != mine {
		t.Fatalf("api_base = %q, want the agent's own", res.Primary.APIBase)
	}
}

// A chain whose primary resolves and whose second entry does not is a chain that
// works until the day it is needed.
func TestTheFallbackChainGetsEndpointsToo(t *testing.T) {
	res := registry.Resolution{
		Primary: modelWith("deepseek", ""),
		Chain: []registry.Model{
			modelWith("openai", ""),
			modelWith("anthropic", "https://custom.example/v1"),
		},
	}
	if err := resolveGanglionEndpoints(&res, config.Agent{Key: "alpha"}, "alpha"); err != nil {
		t.Fatal(err)
	}
	if res.Chain[0].APIBase != "https://api.openai.com/v1" {
		t.Errorf("fallback 0 api_base = %q", res.Chain[0].APIBase)
	}
	if res.Chain[1].APIBase != "https://custom.example/v1" {
		t.Errorf("a fallback's own endpoint was replaced: %q", res.Chain[1].APIBase)
	}
}

// Refused where an operator can see it. The alternative is what actually shipped:
// a config the proxy knew could not work, a harness booting "models: 1 configured",
// and `unsupported protocol scheme ""` in a member's chat.
func TestAnUnresolvableEndpointIsRefusedWithItsCause(t *testing.T) {
	res := registry.Resolution{Primary: modelWith("some-new-provider", "")}
	err := resolveGanglionEndpoints(&res, config.Agent{Key: "alpha"}, "alpha")
	if err == nil {
		t.Fatal("a model with no reachable endpoint was accepted")
	}
	for _, want := range []string{"alpha", "some-new-provider", "baseUrl"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}
