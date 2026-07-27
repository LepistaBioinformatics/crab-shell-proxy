package registry

import (
	"encoding/json"
	"time"
)

// PublicModel is the client-facing shape of a Model. It has NO key field, so a
// handler cannot leak a credential by forgetting to strip one — leaking would
// require adding a field here on purpose.
type PublicModel struct {
	ModelName  string          `json:"model_name"`
	Provider   string          `json:"provider"`
	Model      string          `json:"model"`
	APIBase    string          `json:"api_base,omitempty"`
	AuthMethod string          `json:"auth_method,omitempty"`
	ExtraBody  json.RawMessage `json:"extra_body,omitempty"`

	Status     Status   `json:"status"`
	ReplacedBy string   `json:"replaced_by,omitempty"`
	Fallbacks  []string `json:"fallbacks"`
	Position   int      `json:"position"`

	// HasKey reports whether a credential is stored, which is all a client needs
	// to know to render the "key" badge.
	HasKey bool `json:"has_key"`
	// InUseCount drives the usage column and tells the admin up front why delete
	// and disable are unavailable.
	InUseCount int `json:"in_use_count"`

	ImportedOrphan bool `json:"imported_orphan,omitempty"`

	Version   uint64    `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Public converts a stored record for the wire. Fallbacks is emitted even when
// empty so a client can render the chain column without a null check.
func Public(m Model, inUse int) PublicModel {
	fallbacks := m.Fallbacks
	if fallbacks == nil {
		fallbacks = []string{}
	}
	return PublicModel{
		ModelName: m.ModelName, Provider: m.Provider, Model: m.Model,
		APIBase: m.APIBase, AuthMethod: m.AuthMethod, ExtraBody: m.ExtraBody,
		Status: m.Status, ReplacedBy: m.ReplacedBy, Fallbacks: fallbacks,
		Position: m.Position, HasKey: m.APIKey != "", InUseCount: inUse,
		ImportedOrphan: m.ImportedOrphan,
		Version:        m.Version, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
	}
}
