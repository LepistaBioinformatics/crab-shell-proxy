package docker

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/registry"
)

// fakeModelChecker is a minimal modelNameChecker for tests that need a native
// model_list slot to validate without opening a real registry.
type fakeModelChecker map[string]bool

func (f fakeModelChecker) GetModel(name string) (registry.Model, error) {
	if f[name] {
		return registry.Model{ModelName: name}, nil
	}
	return registry.Model{}, registry.ErrNotFound
}

// writeTestSecurity writes a workspace .security.yml with a pico token and a
// deepseek-chat model_list entry, and returns its path.
func writeTestSecurity(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".security.yml")
	body := "channel_list:\n  pico:\n    settings:\n      token: pico-abc\n" +
		"model_list:\n  deepseek-chat:\n    api_keys:\n      - existing-key\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSecretsRoundTripNamesOnly(t *testing.T) {
	store := t.TempDir()
	sec := writeTestSecurity(t)
	const sentinel = "SUPER-SECRET-VALUE-123"

	cases := []struct{ format, name string }{
		{FormatDotenv, "BRAVE_KEY"},
		{FormatJSON, "OPENAI_KEY"},
		{FormatFile, "token.pem"},
		{FormatNative, "web.brave"},
	}
	for _, c := range cases {
		if err := writeSecret(nil, store, sec, c.format, c.name, sentinel); err != nil {
			t.Fatalf("write %s/%s: %v", c.format, c.name, err)
		}
	}

	names, err := listSecretNames(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(names.Dotenv) != 1 || names.Dotenv[0] != "BRAVE_KEY" {
		t.Errorf("dotenv names = %v", names.Dotenv)
	}
	if len(names.JSON) != 1 || names.JSON[0] != "OPENAI_KEY" {
		t.Errorf("json names = %v", names.JSON)
	}
	if len(names.File) != 1 || names.File[0] != "token.pem" {
		t.Errorf("file names = %v", names.File)
	}
	if len(names.Native) != 1 || names.Native[0] != "web.brave" {
		t.Errorf("native names = %v", names.Native)
	}

	// The listing must NEVER carry a value (write-only-over-API store).
	blob, _ := json.Marshal(names)
	if strings.Contains(string(blob), sentinel) {
		t.Errorf("listing leaked a secret value: %s", blob)
	}
}

func TestValidateSecretName(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "a/b", "a b", "../etc", "a..b", "a$b", "a\nb"} {
		err := validateSecretName(bad)
		if err == nil {
			t.Errorf("name %q should be rejected", bad)
		} else if !errors.Is(err, ErrInvalidSecretName) {
			t.Errorf("name %q: wrong error type %v", bad, err)
		}
	}
	for _, ok := range []string{"BRAVE_KEY", "web.brave", "model_list.deepseek-chat.api_keys", "token.pem"} {
		if err := validateSecretName(ok); err != nil {
			t.Errorf("name %q should be accepted: %v", ok, err)
		}
	}
}

func TestValidateNativeSlotChecksModelsAgainstTheInventory(t *testing.T) {
	at := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	reg, err := registry.Open(filepath.Join(t.TempDir(), "r.db"), func() time.Time { return at })
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	if _, err := reg.CreateModel(registry.Model{
		ModelName: "known", Provider: "openai", Model: "known",
		APIBase: "https://x", APIKey: "sk", Status: registry.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}

	// A model in the inventory is accepted, so a selected model never fails
	// validation.
	if err := validateNativeSlot(reg, "model_list.known.api_keys"); err != nil {
		t.Errorf("known model rejected: %v", err)
	}
	// One that is not is rejected: the inventory is the only place a model exists,
	// so accepting an unknown name would key a credential nothing reads.
	if err := validateNativeSlot(reg, "model_list.ghost.api_keys"); !errors.Is(err, ErrUnknownNativeSlot) {
		t.Errorf("unknown model = %v, want ErrUnknownNativeSlot", err)
	}
	// The web family is unchanged.
	if err := validateNativeSlot(reg, "web.brave"); err != nil {
		t.Errorf("web.brave rejected: %v", err)
	}
	if err := validateNativeSlot(reg, "web.nonsense"); !errors.Is(err, ErrUnknownNativeSlot) {
		t.Errorf("unknown web provider = %v, want ErrUnknownNativeSlot", err)
	}
	// The proxy<->picoclaw channel token must stay unreachable.
	if err := validateNativeSlot(reg, "channel_list.pico.settings.token"); !errors.Is(err, ErrUnknownNativeSlot) {
		t.Errorf("pico token slot = %v, want ErrUnknownNativeSlot", err)
	}
}

func TestDotenvRejectsNewlineValue(t *testing.T) {
	if err := writeSecret(nil, t.TempDir(), "", FormatDotenv, "A", "line1\nINJECTED=x"); !errors.Is(err, ErrInvalidSecretName) {
		t.Errorf("newline dotenv value should be rejected, got %v", err)
	}
}

func TestApplyNativeSecretsPreservesSiblings(t *testing.T) {
	store := t.TempDir()
	sec := writeTestSecurity(t)
	models := fakeModelChecker{"deepseek-chat": true}
	if err := writeSecret(models, store, sec, FormatNative, "web.brave", "brave-key-xyz"); err != nil {
		t.Fatal(err)
	}
	if err := writeSecret(models, store, sec, FormatNative, "model_list.deepseek-chat.api_keys", "new-model-key"); err != nil {
		t.Fatal(err)
	}
	// user "" so chownTree is a no-op (test does not run as root).
	if err := applyNativeSecrets(sec, store, ""); err != nil {
		t.Fatalf("applyNativeSecrets: %v", err)
	}

	// The pico token (proxy↔picoclaw auth) survives the merge.
	if tok, err := readPicoToken(sec); err != nil || tok != "pico-abc" {
		t.Errorf("pico token not preserved: %q err=%v", tok, err)
	}
	m, err := readSecurityConfig(sec)
	if err != nil {
		t.Fatal(err)
	}
	web, _ := m["web"].(map[string]any)
	if web["brave"] != "brave-key-xyz" {
		t.Errorf("web.brave = %v", web["brave"])
	}
	model := m["model_list"].(map[string]any)["deepseek-chat"].(map[string]any)
	keys, _ := model["api_keys"].([]any)
	if len(keys) != 1 || keys[0] != "new-model-key" {
		t.Errorf("model api_keys = %v, want [new-model-key]", keys)
	}
	// After merging native secrets the file is locked content-read-only (0444).
	info, err := os.Stat(sec)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o444 {
		t.Errorf(".security.yml mode = %v, want 0444", info.Mode().Perm())
	}
}

func TestApplyNativeSecretsNoOverlayNoOp(t *testing.T) {
	sec := writeTestSecurity(t)
	before, _ := os.Stat(sec)
	if err := applyNativeSecrets(sec, t.TempDir(), ""); err != nil {
		t.Fatalf("applyNativeSecrets no overlay: %v", err)
	}
	after, _ := os.Stat(sec)
	// Untouched: still 0600, not re-locked to 0444.
	if after.Mode().Perm() != before.Mode().Perm() {
		t.Errorf("mode changed with no overlay: %v -> %v", before.Mode().Perm(), after.Mode().Perm())
	}
}

func TestDeleteSecret(t *testing.T) {
	store := t.TempDir()
	sec := writeTestSecurity(t)
	if err := writeSecret(nil, store, sec, FormatDotenv, "A", "1"); err != nil {
		t.Fatal(err)
	}
	if err := writeSecret(nil, store, sec, FormatDotenv, "B", "2"); err != nil {
		t.Fatal(err)
	}
	if err := deleteSecret(store, sec, "", FormatDotenv, "A"); err != nil {
		t.Fatal(err)
	}
	names, _ := listSecretNames(store)
	if len(names.Dotenv) != 1 || names.Dotenv[0] != "B" {
		t.Errorf("after delete dotenv = %v, want [B]", names.Dotenv)
	}

	// Native delete clears the overlay AND unsets the slot in the workspace.
	if err := writeSecret(nil, store, sec, FormatNative, "web.brave", "k"); err != nil {
		t.Fatal(err)
	}
	if err := applyNativeSecrets(sec, store, ""); err != nil {
		t.Fatal(err)
	}
	if err := deleteSecret(store, sec, "", FormatNative, "web.brave"); err != nil {
		t.Fatal(err)
	}
	m, _ := readSecurityConfig(sec)
	if web, ok := m["web"].(map[string]any); ok {
		if _, exists := web["brave"]; exists {
			t.Error("web.brave not unset in workspace after native delete")
		}
	}
	names, _ = listSecretNames(store)
	if len(names.Native) != 0 {
		t.Errorf("native overlay not cleared after delete: %v", names.Native)
	}
}

func TestUpsertReplacesExisting(t *testing.T) {
	store := t.TempDir()
	sec := writeTestSecurity(t)
	if err := writeSecret(nil, store, sec, FormatDotenv, "K", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := writeSecret(nil, store, sec, FormatDotenv, "K", "v2"); err != nil {
		t.Fatal(err)
	}
	names, _ := listSecretNames(store)
	if len(names.Dotenv) != 1 {
		t.Errorf("upsert must not duplicate a key: %v", names.Dotenv)
	}
	raw, _ := os.ReadFile(filepath.Join(store, ".env"))
	if strings.Contains(string(raw), "v1") {
		t.Errorf(".env kept the stale value: %s", raw)
	}
}
