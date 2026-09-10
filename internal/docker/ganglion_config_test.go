package docker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/registry"
)

func model(name, provider, wire, base, key string) registry.Model {
	return registry.Model{ModelName: name, Provider: provider, Model: wire, APIBase: base, APIKey: key}
}

func decodeDoc(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("the document this proxy writes does not parse: %v\n%s", err, b)
	}
	return doc
}

// THE CONTRACT BETWEEN TWO REPOSITORIES.
//
// The harness derives the same variable name from the same model_name
// (internal/config/file.go, KeyEnvVar). If these two derivations ever disagree
// the agent boots, reads its registry, and skips every candidate for "no API
// key" naming a variable nobody set.
func TestTheKeyVariableNameMatchesTheHarnessDerivation(t *testing.T) {
	for in, want := range map[string]string{
		"primary":         "GANGLION_MODEL_KEY_PRIMARY",
		"gpt-5.4":         "GANGLION_MODEL_KEY_GPT_5_4",
		"own-my model":    "GANGLION_MODEL_KEY_OWN_MY_MODEL",
		"deepseek-chat":   "GANGLION_MODEL_KEY_DEEPSEEK_CHAT",
		"Claude Sonnet 4": "GANGLION_MODEL_KEY_CLAUDE_SONNET_4",
	} {
		if got := ganglionModelKeyEnv(in); got != want {
			t.Errorf("ganglionModelKeyEnv(%q) = %q, want %q", in, got, want)
		}
	}
	if got := ganglionWebKeyEnv("brave"); got != "GANGLION_WEB_KEY_BRAVE" {
		t.Errorf("ganglionWebKeyEnv(brave) = %q", got)
	}
}

func TestTheDocumentIsPicoclawsShape(t *testing.T) {
	b, err := ganglionConfigDoc(registry.Resolution{
		Primary: model("primary", "deepseek", "deepseek-chat", "https://api.deepseek.com/v1", "sk-1"),
		Chain: []registry.Model{
			model("backup", "openai", "gpt-5.4", "https://api.openai.com/v1", "sk-2"),
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	doc := decodeDoc(t, b)

	list := doc["model_list"].([]any)
	if len(list) != 2 {
		t.Fatalf("model_list has %d entries, want 2", len(list))
	}
	first := list[0].(map[string]any)
	for k, want := range map[string]any{
		"model_name": "primary",
		"provider":   "deepseek",
		"model":      "deepseek-chat",
		"api_base":   "https://api.deepseek.com/v1",
		"enabled":    true,
	} {
		if first[k] != want {
			t.Errorf("model_list[0].%s = %v, want %v", k, first[k], want)
		}
	}
	defaults := doc["agents"].(map[string]any)["defaults"].(map[string]any)
	if defaults["model_name"] != "primary" {
		t.Errorf("agents.defaults.model_name = %v", defaults["model_name"])
	}
	if fb := defaults["model_fallbacks"].([]any); len(fb) != 1 || fb[0] != "backup" {
		t.Errorf("agents.defaults.model_fallbacks = %v", fb)
	}
}

// picoclaw splits structure from credentials, and so does this: nothing this
// proxy writes puts a plaintext key on a volume.
func TestNoKeyIsEverWrittenIntoTheFile(t *testing.T) {
	b, err := ganglionConfigDoc(registry.Resolution{
		Primary: model("primary", "deepseek", "deepseek-chat", "https://e/v1", "sk-VERY-SECRET"),
		Chain:   []registry.Model{model("backup", "openai", "gpt", "https://e/v1", "sk-ALSO-SECRET")},
	}, map[string]string{"brave": "BSA-SECRET"})
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"sk-VERY-SECRET", "sk-ALSO-SECRET", "BSA-SECRET", "api_keys", "api_key"} {
		if strings.Contains(string(b), secret) {
			t.Errorf("the config file carries %q:\n%s", secret, b)
		}
	}
}

// Omitted rather than written empty, matching materializeModels' own rule: an
// empty list and an absent key mean the same thing to a reader, and only one of
// them is honest.
func TestNoChainMeansNoFallbacksKey(t *testing.T) {
	b, _ := ganglionConfigDoc(registry.Resolution{
		Primary: model("only", "p", "m", "https://e/v1", "k"),
	}, nil)
	defaults := decodeDoc(t, b)["agents"].(map[string]any)["defaults"].(map[string]any)
	if _, present := defaults["model_fallbacks"]; present {
		t.Fatalf("model_fallbacks was written with no chain:\n%s", b)
	}
}

// A rendered file that is not byte-stable makes the harness re-read its whole
// registry on every ensure and makes a diff useless.
func TestTheRenderIsStable(t *testing.T) {
	res := registry.Resolution{
		Primary: model("primary", "p", "m", "https://e/v1", "k"),
		Chain: []registry.Model{
			model("b", "p", "m", "https://e/v1", "k"),
			model("c", "p", "m", "https://e/v1", "k"),
		},
	}
	web := map[string]string{"tavily": "t", "brave": "b", "kagi": "k"}
	first, _ := ganglionConfigDoc(res, web)
	for i := 0; i < 20; i++ {
		again, _ := ganglionConfigDoc(res, web)
		if string(first) != string(again) {
			t.Fatalf("render %d differed:\n%s\n---\n%s", i, first, again)
		}
	}
}

// Registering a search key once must reach BOTH harnesses. The web.<provider>
// native slot already gives a picoclaw agent its Brave key; reading the same
// slot here is what makes that sentence true rather than aspirational.
func TestAKeyedSearchProviderIsEnabledInTheFile(t *testing.T) {
	b, _ := ganglionConfigDoc(registry.Resolution{
		Primary: model("m", "p", "m", "https://e/v1", "k"),
	}, map[string]string{"brave": "BSA-key", "tavily": ""})
	doc := decodeDoc(t, b)
	web := doc["tools"].(map[string]any)["web"].(map[string]any)
	if brave, ok := web["brave"].(map[string]any); !ok || brave["enabled"] != true {
		t.Errorf("a keyed provider was not enabled: %v", web)
	}
	if _, present := web["tavily"]; present {
		t.Errorf("a provider with an empty key was enabled: %v", web)
	}
}

func TestNoSearchKeysMeansNoToolsBlock(t *testing.T) {
	b, _ := ganglionConfigDoc(registry.Resolution{
		Primary: model("m", "p", "m", "https://e/v1", "k"),
	}, nil)
	if _, present := decodeDoc(t, b)["tools"]; present {
		t.Fatalf("a tools block was written with no provider keyed:\n%s", b)
	}
}

func TestTheSecretEnvironmentCarriesEveryCandidateAndProvider(t *testing.T) {
	env := ganglionSecretEnv(registry.Resolution{
		Primary: model("primary", "p", "m", "https://e/v1", "sk-1"),
		Chain: []registry.Model{
			model("backup", "p", "m", "https://e/v1", "sk-2"),
			model("keyless", "p", "m", "https://e/v1", ""),
		},
	}, map[string]string{"brave": "BSA"})

	joined := strings.Join(env, "\n")
	for _, want := range []string{
		"GANGLION_MODEL_KEY_PRIMARY=sk-1",
		"GANGLION_MODEL_KEY_BACKUP=sk-2",
		"GANGLION_WEB_KEY_BRAVE=BSA",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %s in:\n%s", want, joined)
		}
	}
	// A model with no key gets no variable rather than an empty one: the
	// harness reads an empty value as "no key" either way, and an empty
	// variable is one more thing in `docker inspect` that means nothing.
	if strings.Contains(joined, "KEYLESS") {
		t.Errorf("a keyless model produced a variable:\n%s", joined)
	}
}

// An unstable order would make every ensure look like credential drift and
// recreate the container on every single turn.
func TestTheSecretEnvironmentIsOrdered(t *testing.T) {
	res := registry.Resolution{
		Primary: model("zzz", "p", "m", "https://e/v1", "k"),
		Chain: []registry.Model{
			model("aaa", "p", "m", "https://e/v1", "k"),
			model("mmm", "p", "m", "https://e/v1", "k"),
		},
	}
	web := map[string]string{"tavily": "t", "brave": "b"}
	first := strings.Join(ganglionSecretEnv(res, web), "\n")
	for i := 0; i < 20; i++ {
		if got := strings.Join(ganglionSecretEnv(res, web), "\n"); got != first {
			t.Fatalf("order changed between calls:\n%s\n---\n%s", first, got)
		}
	}
}

// Rewriting an identical file would move its mtime, and the harness re-reads on
// mtime -- so it would re-parse and re-log its whole registry on every turn.
func TestAnUnchangedFileIsNotRewritten(t *testing.T) {
	dir := t.TempDir()
	doc := []byte(`{"model_list":[]}`)

	changed, err := writeGanglionConfig(dir, doc, "")
	if err != nil || !changed {
		t.Fatalf("first write: changed=%v err=%v", changed, err)
	}
	before, err := os.Stat(filepath.Join(dir, ganglionConfigFile))
	if err != nil {
		t.Fatal(err)
	}

	changed, err = writeGanglionConfig(dir, doc, "")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("an identical document was reported as a change")
	}
	after, _ := os.Stat(filepath.Join(dir, ganglionConfigFile))
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("an identical document moved the file's mtime, which the harness watches")
	}

	changed, err = writeGanglionConfig(dir, []byte(`{"model_list":[{"model_name":"x"}]}`), "")
	if err != nil || !changed {
		t.Fatalf("a real change was not reported: changed=%v err=%v", changed, err)
	}
}

func TestNoTemporaryFileIsLeftBehind(t *testing.T) {
	dir := t.TempDir()
	if _, err := writeGanglionConfig(dir, []byte(`{}`), ""); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("left %s behind", e.Name())
		}
	}
}
