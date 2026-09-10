package docker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/registry"
)

// The ganglion harness's own configuration file.
//
// WHY THIS EXISTS AT ALL
//
// Until now a ganglion container's model came from one static field in the
// proxy's config.yaml, shipped as three environment variables at create time.
// The model inventory -- CRUD, the five-level cascade, deprecation with a named
// replacement, per-model fallback chains, members' own models -- reached
// picoclaw agents only, and three gates existed BECAUSE a ganglion agent could
// not consult it.
//
// This is the sink that makes it consultable, and it deliberately reuses
// registry.Resolve rather than teaching the registry about harnesses: ONE
// resolver, two sinks. picoclaw gets config.json + .security.yml; ganglion gets
// this file plus a set of key variables.
//
// WHY THE SHAPE IS PICOCLAW'S
//
// So one admin screen manages both. The harness reads picoclaw's `model_list`,
// `agents.defaults.model_name`, `agents.defaults.model_fallbacks`,
// `agents.defaults.image_model` and `tools.web.*` -- and ignores everything
// else it finds, which means a picoclaw config.json can be handed to it
// verbatim.
//
// WHY THE KEYS ARE NOT IN IT
//
// picoclaw splits structure (config.json) from credentials (.security.yml) and
// so does this: the file carries endpoints and names, the environment carries
// keys, one variable per model. The harness resolves enc:// on both, so an
// operator may still paste an encrypted key into the file by hand -- but
// nothing this proxy writes puts a plaintext credential on a volume.

// ganglionConfigFile is the file's name in the per-user directory. It sits
// BESIDE the workspace rather than inside it: the workspace is the only thing
// the container mounts read-write and the only hierarchy the harness's Landlock
// ruleset grants, so a config file inside it would be one a tool steered by
// untrusted natural language could rewrite -- choosing the endpoint its own
// keys are sent to.
const ganglionConfigFile = ".ganglion-config.json"

// ganglionConfigDest is where that file is bound, read-only, inside the
// container. It matches the harness's own GANGLION_CONFIG_FILE default.
const ganglionConfigDest = ganglionMountDest + "/config.json"

// ganglionModelKeyEnv is the variable carrying one model's key.
//
// THIS DERIVATION MUST MATCH config.KeyEnvVar IN THE HARNESS, character for
// character. The two agreeing is the whole contract between the file and the
// environment; a mismatch surfaces as "model has no API key" naming a variable
// nobody set, which is at least legible, but it is still a broken agent.
func ganglionModelKeyEnv(modelName string) string {
	return "GANGLION_MODEL_KEY_" + envSlug(modelName)
}

// ganglionWebKeyEnv is the variable carrying one search provider's key.
func ganglionWebKeyEnv(provider string) string {
	return "GANGLION_WEB_KEY_" + envSlug(provider)
}

// envSlug upper-cases and replaces everything outside [A-Z0-9] with an
// underscore. Two names that differ only in punctuation therefore collide --
// accepted, because the alternative is an encoding nobody can read in
// `docker inspect` output, which is where these are debugged.
func envSlug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// ganglionConfigDoc renders the file.
//
// Written as an ordinary map rather than a struct so the JSON key names are
// visible in one place beside the harness's own reader -- these two files are a
// wire format between repositories, and a `json:"..."` tag twelve fields down a
// struct is where such a format drifts.
func ganglionConfigDoc(res registry.Resolution, web map[string]string) ([]byte, error) {
	var list []any
	seen := map[string]bool{}
	add := func(m registry.Model) {
		if m.ModelName == "" || seen[m.ModelName] {
			return
		}
		seen[m.ModelName] = true
		entry := map[string]any{
			"model_name": m.ModelName,
			"provider":   m.Provider,
			"model":      m.Model,
			"enabled":    true,
		}
		if m.APIBase != "" {
			entry["api_base"] = m.APIBase
		}
		if m.AuthMethod != "" {
			entry["auth_method"] = m.AuthMethod
		}
		if len(m.ExtraBody) > 0 {
			var eb map[string]json.RawMessage
			if json.Unmarshal(m.ExtraBody, &eb) == nil && len(eb) > 0 {
				entry["extra_body"] = eb
			}
		}
		// NO api_keys. The key travels in the environment; see the file header.
		list = append(list, entry)
	}

	add(res.Primary)
	var fallbacks []string
	for _, m := range res.Chain {
		add(m)
		fallbacks = append(fallbacks, m.ModelName)
	}

	defaults := map[string]any{
		"model_name": res.Primary.ModelName,
	}
	if len(fallbacks) > 0 {
		// Omitted rather than written empty when there is no chain, matching
		// materializeModels' own rule: an empty list and an absent key mean the
		// same thing to a reader and only one of them is honest.
		defaults["model_fallbacks"] = fallbacks
	}

	doc := map[string]any{
		"model_list": list,
		"agents":     map[string]any{"defaults": defaults},
	}
	if tools := ganglionWebBlock(web); tools != nil {
		doc["tools"] = map[string]any{"web": tools}
	}
	return json.MarshalIndent(doc, "", "  ")
}

// ganglionWebBlock turns the native web.<provider> secret slots an admin has
// already filled into an enabled search configuration.
//
// The slots predate this feature: `web.<provider>` is an existing native-secret
// family (secrets.go, validateNativeSlot) that an admin uses to give a PICOCLAW
// agent a Brave key. Reading the same slots here is what makes "register a
// search provider once and both harnesses have it" true rather than aspirational.
//
// A provider with a key is ENABLED. There is no separate switch, deliberately:
// an admin who stored a Brave key wanted Brave used, and a second control whose
// only failure mode is "you keyed it but forgot to tick it" is a support ticket
// waiting to happen. DuckDuckGo is the exception -- it needs no key, so nothing
// here can infer intent, and it is left to the config file to declare.
func ganglionWebBlock(web map[string]string) map[string]any {
	if len(web) == 0 {
		return nil
	}
	out := map[string]any{}
	names := make([]string, 0, len(web))
	for p := range web {
		names = append(names, p)
	}
	// Sorted so the rendered file is stable: an unstable render would make the
	// harness re-read it on every ensure and would make a diff useless.
	sort.Strings(names)
	for _, p := range names {
		if web[p] == "" {
			continue
		}
		out[p] = map[string]any{"enabled": true}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ganglionSecretEnv is the credential half: one variable per model, one per
// search provider.
//
// Sorted for the same reason the file is: this slice becomes a container's
// environment, and an unstable order would make every ensure look like drift
// and recreate the container on every turn.
func ganglionSecretEnv(res registry.Resolution, web map[string]string) []string {
	vars := map[string]string{}
	put := func(m registry.Model) {
		if m.ModelName == "" || m.APIKey == "" {
			return
		}
		vars[ganglionModelKeyEnv(m.ModelName)] = m.APIKey
	}
	put(res.Primary)
	for _, m := range res.Chain {
		put(m)
	}
	for p, key := range web {
		if key != "" {
			vars[ganglionWebKeyEnv(p)] = key
		}
	}

	names := make([]string, 0, len(vars))
	for n := range vars {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, n+"="+vars[n])
	}
	return out
}

// writeGanglionConfig writes the file and reports whether its bytes changed.
//
// The bool is what the harness's mtime watch keys off indirectly and what keeps
// this cheap: rewriting an identical file on every ensure would move its mtime,
// and the harness would re-read and re-log its whole registry on every single
// turn.
func writeGanglionConfig(userDir string, doc []byte, user string) (bool, error) {
	path := filepath.Join(userDir, ganglionConfigFile)
	if old, err := os.ReadFile(path); err == nil && string(old) == string(doc) {
		return false, nil
	}
	// Atomic: the harness re-reads on mtime, so a half-written file is a file
	// it would try to parse. It keeps the previous configuration when the parse
	// fails, but relying on that would mean relying on a recovery path for an
	// ordinary write.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, doc, 0o600); err != nil {
		return false, fmt.Errorf("write ganglion config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return false, fmt.Errorf("write ganglion config: %w", err)
	}
	if err := chownTree(path, user); err != nil {
		return false, fmt.Errorf("chown ganglion config: %w", err)
	}
	return true, nil
}

// ganglionWebSecrets reads the native web.<provider> slots for one workspace.
//
// Returns an empty map rather than an error when there is no overlay: no
// secrets configured is the ordinary case, not a fault.
func ganglionWebSecrets(storeDir string) (map[string]string, error) {
	overlay, err := readOverlay(filepath.Join(storeDir, "native.yml"))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for slot, value := range overlay {
		name, ok := strings.CutPrefix(slot, "web.")
		if !ok || value == "" {
			continue
		}
		out[name] = value
	}
	return out, nil
}
