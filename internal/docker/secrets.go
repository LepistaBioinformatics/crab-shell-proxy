package docker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/registry"
	"gopkg.in/yaml.v3"
)

// Secret sink formats. The caller picks the format so the injected secret lands
// where its consuming skill reads it (CTX-AC-02).
const (
	FormatDotenv = "dotenv" // .env          — NAME=value lines
	FormatJSON   = "json"   // secrets.json  — { "NAME": "value" }
	FormatFile   = "file"   // secrets/<NAME> — one file per secret, content = value
	FormatNative = "native" // native.yml overlay merged into .security.yml slots
)

// ErrInvalidSecretName and ErrUnknownNativeSlot are returned by the write path
// for a bad name/format or an unrecognized native slot; handlers map both to 400.
var (
	ErrInvalidSecretName = errors.New("invalid secret name")
	ErrUnknownNativeSlot = errors.New("unknown native slot")
)

// errModelNotInWorkspace signals that a model_list slot named a model the
// INVENTORY knows (it passed validateNativeSlot) but that is not part of THIS
// workspace's materialized model_list — e.g. a scope-level secret set for a
// model this workspace was never assigned. It is internal to
// setNativeSlot/applyNativeSecrets: the accept-set (the inventory) is
// necessarily wider than any one workspace's apply-set, and this is how
// applyNativeSecrets tells "not applicable here" apart from a real failure.
var errModelNotInWorkspace = errors.New("model not in this workspace's model_list")

// SecretNames is the set of stored secret NAMES per format — never the values
// (write-only-over-API store, CTX-AC-02). It is the GET /v1/secrets response.
type SecretNames struct {
	Dotenv []string `json:"dotenv"`
	JSON   []string `json:"json"`
	Native []string `json:"native"`
	File   []string `json:"file"`
}

// webProviders is the fixed enum of picoclaw web-search slots (context.md); a
// native web.<provider> slot is valid iff its provider is in this set.
var webProviders = map[string]bool{
	"brave": true, "tavily": true, "kagi": true, "gemini": true,
	"perplexity": true, "glm_search": true, "baidu_search": true,
}

var secretNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// validateSecretName enforces the safe charset and rejects the traversal-prone
// "", ".", ".." and any name containing "..", so a name can never escape the
// store dir (file sink) or address an unintended path.
func validateSecretName(name string) error {
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("%w: %q", ErrInvalidSecretName, name)
	}
	if !secretNameRe.MatchString(name) {
		return fmt.Errorf("%w: %q (allowed charset: A-Za-z0-9._-)", ErrInvalidSecretName, name)
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("%w: %q must not contain %q", ErrInvalidSecretName, name, "..")
	}
	return nil
}

// writeSecret upserts one secret into the store under the chosen format. For
// native it first validates the slot against the model inventory; the caller
// merges the overlay into the workspace separately.
func writeSecret(models modelNameChecker, storeDir, secPath, format, name, value string) error {
	switch format {
	case FormatDotenv:
		if strings.ContainsAny(value, "\n\r") {
			return fmt.Errorf("%w: dotenv value must not contain newlines", ErrInvalidSecretName)
		}
		return upsertDotenv(filepath.Join(storeDir, ".env"), name, value)
	case FormatJSON:
		return upsertJSON(filepath.Join(storeDir, "secrets.json"), name, value)
	case FormatFile:
		return writeFileSecret(filepath.Join(storeDir, "secrets"), name, value)
	case FormatNative:
		if err := validateNativeSlot(models, name); err != nil {
			return err
		}
		return upsertOverlay(filepath.Join(storeDir, "native.yml"), name, value)
	default:
		return fmt.Errorf("%w: unknown format %q", ErrInvalidSecretName, format)
	}
}

// deleteSecret removes one secret from the store; for native it also unsets the
// slot in the caller's current workspace .security.yml (when provisioned).
func deleteSecret(storeDir, secPath, user, format, name string) error {
	switch format {
	case FormatDotenv:
		return deleteDotenv(filepath.Join(storeDir, ".env"), name)
	case FormatJSON:
		return deleteJSON(filepath.Join(storeDir, "secrets.json"), name)
	case FormatFile:
		return deleteFileSecret(filepath.Join(storeDir, "secrets"), name)
	case FormatNative:
		if err := deleteOverlay(filepath.Join(storeDir, "native.yml"), name); err != nil {
			return err
		}
		if _, err := os.Stat(secPath); err != nil {
			return nil // workspace not provisioned yet: nothing merged to unset
		}
		sec, err := readSecurityConfig(secPath)
		if err != nil {
			return err
		}
		unsetNativeSlot(sec, name)
		return writeSecurityConfig(secPath, sec, user)
	default:
		return fmt.Errorf("%w: unknown format %q", ErrInvalidSecretName, format)
	}
}

// listSecretNames parses each sink server-side and returns the names only, never
// a value. Names are sorted for a stable response.
func listSecretNames(storeDir string) (SecretNames, error) {
	names := SecretNames{Dotenv: []string{}, JSON: []string{}, Native: []string{}, File: []string{}}

	lines, err := readLines(filepath.Join(storeDir, ".env"))
	if err != nil {
		return names, err
	}
	for _, l := range lines {
		if k, _, ok := strings.Cut(l, "="); ok {
			if k = strings.TrimSpace(k); k != "" {
				names.Dotenv = append(names.Dotenv, k)
			}
		}
	}

	jm, err := readJSONMap(filepath.Join(storeDir, "secrets.json"))
	if err != nil {
		return names, err
	}
	for k := range jm {
		names.JSON = append(names.JSON, k)
	}

	om, err := readOverlay(filepath.Join(storeDir, "native.yml"))
	if err != nil {
		return names, err
	}
	for k := range om {
		names.Native = append(names.Native, k)
	}

	entries, err := os.ReadDir(filepath.Join(storeDir, "secrets"))
	if err != nil && !os.IsNotExist(err) {
		return names, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			names.File = append(names.File, e.Name())
		}
	}

	sort.Strings(names.Dotenv)
	sort.Strings(names.JSON)
	sort.Strings(names.Native)
	sort.Strings(names.File)
	return names, nil
}

// modelNameChecker is the slice of the registry this validation needs. Taking an
// interface keeps secrets.go testable without a Manager and documents that the
// only question being asked is "does this model exist".
type modelNameChecker interface {
	GetModel(name string) (registry.Model, error)
}

// validateNativeSlot accepts only two families: web.<provider> from the fixed
// enum, and model_list.<model>.api_keys where the model exists in the INVENTORY.
// Everything else — notably channel_list.pico.settings.token, the
// proxy<->picoclaw auth token — is rejected so a user can never overwrite it.
//
// The model check reads the inventory rather than a template's .security.yml: the
// inventory is now the only place a model exists, so a name it does not know would
// key a credential nothing reads. A model registered through the admin UI
// therefore always passes, and a typo never does.
//
// A slot that passes becomes a scope-level OVERLAY over the inventory's own key:
// applyNativeSecrets runs after materialization, so a scope admin can supply their
// own credential for a registered model. That is a layered override with defined
// precedence, not a second writer.
func validateNativeSlot(models modelNameChecker, slot string) error {
	parts := strings.Split(slot, ".")
	switch {
	case len(parts) == 2 && parts[0] == "web":
		if webProviders[parts[1]] {
			return nil
		}
		return fmt.Errorf("%w: unknown web provider %q", ErrUnknownNativeSlot, parts[1])
	case len(parts) == 3 && parts[0] == "model_list" && parts[2] == "api_keys":
		if _, err := models.GetModel(parts[1]); err != nil {
			return fmt.Errorf("%w: model %q is not in the model inventory", ErrUnknownNativeSlot, parts[1])
		}
		return nil
	}
	return fmt.Errorf("%w: %q", ErrUnknownNativeSlot, slot)
}

// isNativeModelSlot reports whether a native slot addresses a model's api_keys —
// the family that must target a single agent's workspace set, so it cannot
// be published to an all-agents scope (native-secrets-admin-only FR-4).
func isNativeModelSlot(slot string) bool {
	parts := strings.Split(slot, ".")
	return len(parts) == 3 && parts[0] == "model_list" && parts[2] == "api_keys"
}

// applyNativeSecrets merges the native.yml overlay from storeDir into secPath's
// .security.yml at the named slots (preserving all sibling keys — the pico token
// and model api_keys survive), then re-locks the file 0444. No-op when no native
// secret is set. Idempotent, run on every ensure.
//
// A model_list slot can validly name a model this particular workspace never
// resolved: the inventory (the accept-set) is wider than any one workspace's
// materialized model_list (the apply-set) by design — a scope-level secret is
// set once for models that may only be assigned to some workspaces, including
// ones that don't exist yet. That slot is skipped and logged, not treated as a
// failure; every other error still aborts the merge (so one bad slot never
// silently drops the rest, e.g. a working web.* entry, from being written).
// logf is required to record a skip; pass nil to no-op it (mirrors NewManager).
func applyNativeSecrets(secPath, storeDir, user string, logf func(string, ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	overlay, err := readOverlay(filepath.Join(storeDir, "native.yml"))
	if err != nil {
		return err
	}
	if len(overlay) == 0 {
		return nil
	}
	sec, err := readSecurityConfig(secPath)
	if err != nil {
		return err
	}
	for slot, value := range overlay {
		if err := setNativeSlot(sec, slot, value); err != nil {
			if errors.Is(err, errModelNotInWorkspace) {
				logf("native secret %q not applied to %s: %v", slot, secPath, err)
				continue
			}
			return err
		}
	}
	return writeSecurityConfig(secPath, sec, user)
}

// setNativeSlot deep-sets the dotted slot into the parsed config, creating only
// the web section on demand and never replacing a parent map wholesale.
func setNativeSlot(sec map[string]any, slot, value string) error {
	parts := strings.Split(slot, ".")
	switch {
	case len(parts) == 2 && parts[0] == "web":
		childMap(sec, "web")[parts[1]] = value
		return nil
	case len(parts) == 3 && parts[0] == "model_list" && parts[2] == "api_keys":
		ml, ok := sec["model_list"].(map[string]any)
		if !ok {
			return fmt.Errorf("%w: model_list absent from this workspace", errModelNotInWorkspace)
		}
		model, ok := ml[parts[1]].(map[string]any)
		if !ok {
			return fmt.Errorf("%w: model %q absent from this workspace", errModelNotInWorkspace, parts[1])
		}
		model["api_keys"] = []string{value}
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrUnknownNativeSlot, slot)
	}
}

// unsetNativeSlot removes the dotted slot from the parsed config, leaving all
// siblings intact.
func unsetNativeSlot(sec map[string]any, slot string) {
	parts := strings.Split(slot, ".")
	switch {
	case len(parts) == 2 && parts[0] == "web":
		if web, ok := sec["web"].(map[string]any); ok {
			delete(web, parts[1])
		}
	case len(parts) == 3 && parts[0] == "model_list" && parts[2] == "api_keys":
		if ml, ok := sec["model_list"].(map[string]any); ok {
			if model, ok := ml[parts[1]].(map[string]any); ok {
				delete(model, "api_keys")
			}
		}
	}
}

func childMap(m map[string]any, key string) map[string]any {
	if existing, ok := m[key].(map[string]any); ok {
		return existing
	}
	child := map[string]any{}
	m[key] = child
	return child
}

func readSecurityConfig(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// writeSecurityConfig rewrites the workspace .security.yml, chowns it to the
// non-root agent, and re-locks it 0444 (content-read-only — picoclaw does not
// rewrite it at runtime, design §3 R-A). The proxy runs as root so it can always
// rewrite despite the 0444 from a prior merge; relax then re-lock to be robust.
func writeSecurityConfig(secPath string, sec map[string]any, user string) error {
	out, err := yaml.Marshal(sec)
	if err != nil {
		return err
	}
	_ = os.Chmod(secPath, 0o600)
	if err := os.WriteFile(secPath, out, 0o600); err != nil {
		return err
	}
	if err := chownTree(secPath, user); err != nil {
		return err
	}
	return os.Chmod(secPath, 0o444)
}

// --- dotenv sink ---

func upsertDotenv(path, name, value string) error {
	lines, err := readLines(path)
	if err != nil {
		return err
	}
	newLine := name + "=" + value
	found := false
	for i, l := range lines {
		if k, _, ok := strings.Cut(l, "="); ok && strings.TrimSpace(k) == name {
			lines[i] = newLine
			found = true
			break
		}
	}
	if !found {
		lines = append(lines, newLine)
	}
	return writeLines(path, lines)
}

func deleteDotenv(path, name string) error {
	lines, err := readLines(path)
	if err != nil {
		return err
	}
	out := lines[:0]
	for _, l := range lines {
		if k, _, ok := strings.Cut(l, "="); ok && strings.TrimSpace(k) == name {
			continue
		}
		out = append(out, l)
	}
	return writeLines(path, out)
}

func readLines(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var lines []string
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		lines = append(lines, l)
	}
	return lines, nil
}

func writeLines(path string, lines []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var sb strings.Builder
	for _, l := range lines {
		sb.WriteString(l)
		sb.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(sb.String()), 0o600)
}

// --- json sink ---

func upsertJSON(path, name, value string) error {
	m, err := readJSONMap(path)
	if err != nil {
		return err
	}
	m[name] = value
	return writeJSONMap(path, m)
}

func deleteJSON(path, name string) error {
	m, err := readJSONMap(path)
	if err != nil {
		return err
	}
	delete(m, name)
	return writeJSONMap(path, m)
}

func readJSONMap(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	m := map[string]string{}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func writeJSONMap(path string, m map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
}

// --- file sink ---

func writeFileSecret(dir, name, value string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600)
}

func deleteFileSecret(dir, name string) error {
	if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// --- native overlay (store side) ---

func upsertOverlay(path, slot, value string) error {
	m, err := readOverlay(path)
	if err != nil {
		return err
	}
	if m == nil {
		m = map[string]string{}
	}
	m[slot] = value
	return writeOverlay(path, m)
}

func deleteOverlay(path, slot string) error {
	m, err := readOverlay(path)
	if err != nil {
		return err
	}
	delete(m, slot)
	return writeOverlay(path, m)
}

func readOverlay(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var m map[string]string
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func writeOverlay(path string, m map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	out, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
}
