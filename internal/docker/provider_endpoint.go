package docker

// The provider -> endpoint table, for a harness that needs one written down.
//
// picoclaw resolves a provider name to an endpoint ITSELF, so its configuration
// never had to carry one: `provider: "deepseek"` was a complete answer. The
// ganglion has no such table -- it is given a base url and posts to it -- so every
// agent migrated from picoclaw arrives with a model that names a provider, names
// no endpoint, and fails on its first turn with `unsupported protocol scheme ""`.
//
// DERIVED FROM THE EMBEDDED CATALOG rather than written out again here. The
// catalog already pairs a provider with its api_base for the admin's register
// form, and two lists of endpoints drift -- the second one silently, because
// nothing compares them. Its doc comment said the catalog is "never copied into a
// workspace"; that boundary is deliberately widened here, and the widening is
// narrow: the catalog supplies an endpoint ONLY when neither the inventory nor the
// agent's own configuration did.

import (
	"strings"
	"sync"
)

// templateProviders are catalog entries whose api_base is a SHAPE, not an address.
//
// Azure's is `https://your-resource.openai.azure.com` -- a fill-in-the-blank the
// admin form shows and nobody can post to. Using it as a fallback would turn a
// missing endpoint into a DNS failure against a hostname that never existed,
// which is a worse error than the honest refusal.
var templateProviders = map[string]bool{"azure": true}

var (
	endpointOnce sync.Once
	endpoints    map[string]string
)

// ProviderEndpoint is the default api_base for a provider, or "" when the catalog
// has none and the operator has to say.
//
// Case-insensitive, because a provider name arrives from a config file an operator
// typed.
//
// Note for the local runtimes -- ollama, lmstudio, vllm, github-copilot -- whose
// catalog entry is a localhost address: inside a container that is the CONTAINER,
// not the host, so an operator using one of them sets an explicit baseUrl. That is
// equally true under picoclaw and is not something this table can fix.
func ProviderEndpoint(provider string) string {
	endpointOnce.Do(func() {
		endpoints = map[string]string{}
		entries, err := SuggestionCatalog()
		if err != nil {
			return // the catalog is embedded; a failure here means no fallback, not a panic
		}
		for _, e := range entries {
			name := strings.ToLower(strings.TrimSpace(e.Provider))
			if name == "" || e.APIBase == "" || templateProviders[name] {
				continue
			}
			// FIRST entry wins. The catalog lists several models per provider and
			// they agree on the endpoint; taking the first keeps this stable if one
			// ever does not, rather than depending on iteration order.
			if _, seen := endpoints[name]; !seen {
				endpoints[name] = e.APIBase
			}
		}
	})
	return endpoints[strings.ToLower(strings.TrimSpace(provider))]
}
