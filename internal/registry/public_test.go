package registry

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestPublicModelCannotCarryAKey(t *testing.T) {
	m := Model{
		ModelName: "m", Provider: "openai", Model: "gpt-5.4",
		APIBase: "https://api.openai.com/v1", APIKey: "sk-super-secret",
		Status: StatusActive, Version: 3,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}

	raw, err := json.Marshal(Public(m, 2))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// The wire type has no key field at all, so leaking one requires ADDING a
	// field rather than forgetting to strip one.
	if strings.Contains(string(raw), "sk-super-secret") || strings.Contains(string(raw), "api_key") {
		t.Fatalf("PublicModel leaked the key: %s", raw)
	}

	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out["has_key"] != true {
		t.Errorf("has_key = %#v, want true", out["has_key"])
	}
	if out["in_use_count"] != float64(2) {
		t.Errorf("in_use_count = %#v, want 2", out["in_use_count"])
	}
	if out["model_name"] != "m" || out["version"] != float64(3) {
		t.Errorf("public model = %#v", out)
	}
}

func TestPublicModelReportsNoKeyWhenAbsent(t *testing.T) {
	p := Public(Model{ModelName: "oauth", Provider: "antigravity", Model: "g", AuthMethod: "oauth"}, 0)
	if p.HasKey {
		t.Error("HasKey must be false when no key is stored")
	}
}
