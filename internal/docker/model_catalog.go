package docker

import (
	"embed"
	"encoding/json"
	"fmt"
	"sync"
)

//go:embed model-catalog.json
var catalogFS embed.FS

// CatalogEntry is one suggested model definition. It never carries a key or a
// model_name: the key is the admin's secret and the name is the admin's choice,
// which must be unique in the inventory.
type CatalogEntry struct {
	Provider   string          `json:"provider"`
	Model      string          `json:"model"`
	APIBase    string          `json:"api_base,omitempty"`
	AuthMethod string          `json:"auth_method,omitempty"`
	ExtraBody  json.RawMessage `json:"extra_body,omitempty"`
}

var (
	catalogOnce sync.Once
	catalog     []CatalogEntry
	catalogErr  error
)

// SuggestionCatalog returns the embedded read-only catalog used to prefill the
// admin's register form, replacing the five free-text inputs that made typos the
// normal failure mode. It is never copied into a workspace — a workspace's
// model_list comes only from the inventory.
func SuggestionCatalog() ([]CatalogEntry, error) {
	catalogOnce.Do(func() {
		raw, err := catalogFS.ReadFile("model-catalog.json")
		if err != nil {
			catalogErr = fmt.Errorf("read embedded model catalog: %w", err)
			return
		}
		if err := json.Unmarshal(raw, &catalog); err != nil {
			catalogErr = fmt.Errorf("parse embedded model catalog: %w", err)
		}
	})
	return catalog, catalogErr
}
