package docker

import (
	"os"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/registry"
)

// The bytes on disk are where user-owned-models R5 actually lives: the promise
// that a member's own model degrades to their organisation's instead of leaving
// them with an agent that cannot answer is picoclaw's own
// agents.defaults.model_fallbacks, and nothing above this function can check it.
//
// materializeModels is a plain function over two paths, so these run without the
// root privileges the Manager-level tests need.

func ownResolution() registry.Resolution {
	own := registry.UserModel{
		OwnerAccID: "u1", Slug: "mine", Provider: "groq", Model: "llama-3.3-70b",
		APIBase: "https://api.groq.com/openai/v1", APIKey: "sk-mine", Enabled: true,
	}
	return registry.Resolution{
		Primary: registry.Model{
			ModelName: own.MaterializedName(), Provider: own.Provider, Model: own.Model,
			APIBase: own.APIBase, APIKey: own.APIKey, Status: registry.StatusActive,
		},
		Chain: []registry.Model{{
			ModelName: "org", Provider: "openai", Model: "gpt-5.4",
			APIBase: "https://api.openai.com/v1", APIKey: "sk-org", Status: registry.StatusActive,
		}},
		Level:       registry.LevelUserModel,
		UserModel:   "u1/mine",
		CascadeName: "org",
	}
}

func TestMaterializeRunsThePersonalModelWithTheOrganisationsAsFallback(t *testing.T) {
	_, configPath, secPath := seedWorkspaceFiles(t)

	if err := materializeModels(configPath, secPath, ownResolution(), projectList{}); err != nil {
		t.Fatalf("materializeModels: %v", err)
	}

	cfg := readConfig(t, configPath)
	defaults := cfg["agents"].(map[string]any)["defaults"].(map[string]any)
	if defaults["model_name"] != registry.OwnPrefix+"mine" || defaults["provider"] != "groq" {
		t.Errorf("defaults = %#v, want the member's own model primary", defaults)
	}
	// THE fallback. Without it a failing personal key stops the agent answering,
	// which is the whole thing this chain exists to prevent.
	fb, ok := defaults["model_fallbacks"].([]any)
	if !ok || len(fb) != 1 || fb[0] != "org" {
		t.Fatalf("model_fallbacks = %#v, want [org]", defaults["model_fallbacks"])
	}

	list, ok := cfg["model_list"].([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("model_list = %#v, want both models declared", cfg["model_list"])
	}
	// A fallback picoclaw cannot look up is not a fallback.
	names := map[string]bool{}
	for _, item := range list {
		entry := item.(map[string]any)
		names[entry["model_name"].(string)] = true
		if _, leaked := entry["api_key"]; leaked {
			t.Errorf("model_list entry carries a key: %#v", entry)
		}
	}
	if !names[registry.OwnPrefix+"mine"] || !names["org"] {
		t.Errorf("model_list names = %v, want both own-mine and org", names)
	}
}

func TestMaterializeWritesBothKeysSoTheFallbackCanAuthenticate(t *testing.T) {
	_, configPath, secPath := seedWorkspaceFiles(t)

	if err := materializeModels(configPath, secPath, ownResolution(), projectList{}); err != nil {
		t.Fatalf("materializeModels: %v", err)
	}

	raw, err := os.ReadFile(secPath)
	if err != nil {
		t.Fatal(err)
	}
	sec := string(raw)
	// The organisation's key has to be on disk too: a fallback that cannot
	// authenticate turns "degrade to the org model" into a second failure.
	for _, want := range []string{"sk-mine", "sk-org", registry.OwnPrefix + "mine"} {
		if !strings.Contains(sec, want) {
			t.Errorf(".security.yml is missing %q:\n%s", want, sec)
		}
	}
	// The previous model's key is pruned, and the unrelated slots are not.
	if strings.Contains(sec, "sk-old") {
		t.Errorf("a retired model's key survived:\n%s", sec)
	}
	for _, keep := range []string{"pico-seed", "brave-key"} {
		if !strings.Contains(sec, keep) {
			t.Errorf("materialization clobbered %q:\n%s", keep, sec)
		}
	}
}

// A member with their own key on an instance whose administrator set no default
// at all: there is nothing to fall back to, and a stale chain would be worse
// than none — picoclaw would name a model its model_list does not declare.
func TestMaterializeClearsTheChainWhenTheCascadeResolvesNothing(t *testing.T) {
	_, configPath, secPath := seedWorkspaceFiles(t)

	// First materialize WITH a fallback, so there is a chain to leave behind.
	if err := materializeModels(configPath, secPath, ownResolution(), projectList{}); err != nil {
		t.Fatalf("materializeModels: %v", err)
	}

	alone := ownResolution()
	alone.Chain = nil
	alone.CascadeName = ""
	if err := materializeModels(configPath, secPath, alone, projectList{}); err != nil {
		t.Fatalf("materializeModels: %v", err)
	}

	cfg := readConfig(t, configPath)
	defaults := cfg["agents"].(map[string]any)["defaults"].(map[string]any)
	if _, present := defaults["model_fallbacks"]; present {
		t.Errorf("model_fallbacks = %#v, want it gone", defaults["model_fallbacks"])
	}
	if list := cfg["model_list"].([]any); len(list) != 1 {
		t.Errorf("model_list = %#v, want only the personal model", list)
	}
	raw, _ := os.ReadFile(secPath)
	if strings.Contains(string(raw), "sk-org") {
		t.Errorf("the former fallback's key survived:\n%s", raw)
	}
}
