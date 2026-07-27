package docker

import "testing"

func TestSuggestionCatalogParsesAndCarriesNoKeys(t *testing.T) {
	entries, err := SuggestionCatalog()
	if err != nil {
		t.Fatalf("SuggestionCatalog: %v", err)
	}
	if len(entries) < 20 {
		t.Fatalf("catalog has %d entries, want the full set (~30)", len(entries))
	}
	for i, e := range entries {
		if e.Provider == "" || e.Model == "" {
			t.Errorf("entry %d incomplete: %+v", i, e)
		}
		// Either an api_base or an auth_method must be present, or the entry
		// cannot prefill anything usable.
		if e.APIBase == "" && e.AuthMethod == "" {
			t.Errorf("entry %d has neither api_base nor auth_method: %+v", i, e)
		}
	}
}

func TestSuggestionCatalogIncludesKnownProviders(t *testing.T) {
	entries, err := SuggestionCatalog()
	if err != nil {
		t.Fatalf("SuggestionCatalog: %v", err)
	}
	want := map[string]bool{"openai": false, "anthropic": false, "zhipu": false, "ollama": false}
	for _, e := range entries {
		if _, tracked := want[e.Provider]; tracked {
			want[e.Provider] = true
		}
	}
	for p, found := range want {
		if !found {
			t.Errorf("catalog is missing provider %q", p)
		}
	}
}
