package docker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/registry"
)

func TestNormalizeDiskTemplatesEmptiesModelListAndBacksUp(t *testing.T) {
	m, _, root := testManagerWithRegistry(t)
	tmplDir := config.TemplatesDir(root, "picoclaw")
	if err := os.MkdirAll(tmplDir, 0o700); err != nil {
		t.Fatal(err)
	}
	original := `{"version":3,"agents":{"defaults":{"provider":"zhipu","model_name":"glm-4.7"}},` +
		`"model_list":[{"model_name":"glm-4.7","provider":"zhipu","model":"glm-4.7"}]}`
	tmplPath := filepath.Join(tmplDir, "config.json")
	if err := os.WriteFile(tmplPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	m.cfg.Agents = map[string]config.Agent{"alpha": {Key: "alpha", Template: "picoclaw"}}

	if err := m.normalizeDiskTemplates(); err != nil {
		t.Fatalf("normalizeDiskTemplates: %v", err)
	}

	raw, _ := os.ReadFile(tmplPath)
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	list, ok := cfg["model_list"].([]any)
	if !ok || len(list) != 0 {
		t.Errorf("model_list = %#v, want []", cfg["model_list"])
	}
	defaults := cfg["agents"].(map[string]any)["defaults"].(map[string]any)
	if defaults["provider"] != "" || defaults["model_name"] != "" {
		t.Errorf("defaults = %#v, want both cleared", defaults)
	}

	// This is the migration's only destructive write, so it must be reversible by
	// hand.
	backup, err := os.ReadFile(tmplPath + ".pre-registry")
	if err != nil {
		t.Fatalf("backup missing: %v", err)
	}
	if string(backup) != original {
		t.Errorf("backup = %s, want the original verbatim", backup)
	}
}

func TestNormalizeDiskTemplatesCoversATemplateNoAgentDeclares(t *testing.T) {
	// config.Load drops disabled or removed agents from m.cfg.Agents, so a
	// template used only by such an agent must still be found and normalized —
	// otherwise it stays un-normalized, still carrying models on disk.
	m, _, root := testManagerWithRegistry(t)
	tmplDir := config.TemplatesDir(root, "orphaned")
	if err := os.MkdirAll(tmplDir, 0o700); err != nil {
		t.Fatal(err)
	}
	original := `{"version":3,"agents":{"defaults":{"provider":"zhipu","model_name":"glm-4.7"}},` +
		`"model_list":[{"model_name":"glm-4.7","provider":"zhipu","model":"glm-4.7"}]}`
	tmplPath := filepath.Join(tmplDir, "config.json")
	if err := os.WriteFile(tmplPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	// No agent in m.cfg.Agents names "orphaned" as its Template.
	m.cfg.Agents = map[string]config.Agent{"alpha": {Key: "alpha", Template: "picoclaw"}}

	if err := m.normalizeDiskTemplates(); err != nil {
		t.Fatalf("normalizeDiskTemplates: %v", err)
	}

	raw, _ := os.ReadFile(tmplPath)
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	list, ok := cfg["model_list"].([]any)
	if !ok || len(list) != 0 {
		t.Errorf("model_list = %#v, want [] (template not reachable via m.cfg.Agents was skipped)", cfg["model_list"])
	}
	if _, err := os.ReadFile(tmplPath + ".pre-registry"); err != nil {
		t.Errorf("backup missing for a template no agent declares: %v", err)
	}
}

func TestNormalizeDiskTemplatesDoesNotOverwriteAnExistingBackup(t *testing.T) {
	m, _, root := testManagerWithRegistry(t)
	tmplDir := config.TemplatesDir(root, "picoclaw")
	if err := os.MkdirAll(tmplDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tmplPath := filepath.Join(tmplDir, "config.json")
	if err := os.WriteFile(tmplPath, []byte(`{"model_list":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmplPath+".pre-registry", []byte(`{"original":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m.cfg.Agents = map[string]config.Agent{"alpha": {Key: "alpha", Template: "picoclaw"}}

	if err := m.normalizeDiskTemplates(); err != nil {
		t.Fatalf("normalizeDiskTemplates: %v", err)
	}

	// A re-run must not replace the real original with an already-normalized copy.
	backup, _ := os.ReadFile(tmplPath + ".pre-registry")
	if string(backup) != `{"original":true}` {
		t.Errorf("backup was overwritten: %s", backup)
	}
}

func TestCheckModelDriftLogsAMismatchAndStaysSilentWhenClean(t *testing.T) {
	m, reg, root := testManagerWithRegistry(t)
	var logged []string
	m.logf = func(format string, args ...any) {
		logged = append(logged, format)
	}
	m.cfg.Agents = map[string]config.Agent{"alpha": {Key: "alpha", Template: "picoclaw"}}

	key := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"}
	seedLegacyWorkspace(t, root, key, "ondisk", "openai", "sk-ondisk", nil)
	if err := reg.PutAssignment(m.workspaceRef(key), registry.Assignment{
		ModelName: "recorded", Source: registry.SourceInherited,
	}); err != nil {
		t.Fatal(err)
	}

	m.checkModelDrift()

	var found bool
	for _, l := range logged {
		if strings.Contains(l, "drift") {
			found = true
		}
	}
	if !found {
		t.Errorf("a mismatch must be logged; logged = %v", logged)
	}

	// Correcting the record clears the report — the check is read-only, so it must
	// reflect state rather than remember a past complaint.
	logged = nil
	if err := reg.PutAssignment(m.workspaceRef(key), registry.Assignment{
		ModelName: "ondisk", Source: registry.SourceInherited,
	}); err != nil {
		t.Fatal(err)
	}
	m.checkModelDrift()
	for _, l := range logged {
		if strings.Contains(l, "drift") {
			t.Errorf("clean state still reported drift: %v", logged)
		}
	}
}

func TestCheckModelDriftDoesNotModifyAnything(t *testing.T) {
	m, reg, root := testManagerWithRegistry(t)
	m.cfg.Agents = map[string]config.Agent{"alpha": {Key: "alpha", Template: "picoclaw"}}
	key := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"}
	userDir := seedLegacyWorkspace(t, root, key, "ondisk", "openai", "sk-ondisk", nil)
	if err := reg.PutAssignment(m.workspaceRef(key), registry.Assignment{
		ModelName: "recorded", Source: registry.SourceInherited,
	}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(userDir, "config.json"))

	m.checkModelDrift()

	after, _ := os.ReadFile(filepath.Join(userDir, "config.json"))
	if string(before) != string(after) {
		t.Error("drift check rewrote a workspace; a correction is an explicit admin reapply")
	}
	a, _ := reg.GetAssignment(m.workspaceRef(key))
	if a.ModelName != "recorded" {
		t.Error("drift check rewrote the assignment")
	}
}
