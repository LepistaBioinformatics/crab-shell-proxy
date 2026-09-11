package docker

import (
	"encoding/json"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/registry"
)

// AC-8. ONE inventory record reaches BOTH harnesses under picoclaw's own key.
//
// That is the whole reason the field lives on the inventory rather than on a
// harness: thinking_level describes what an ENDPOINT will do when asked to
// reason, which does not change because a different binary is asking.
func TestThinkingLevelReachesBothHarnessesFromOneRecord(t *testing.T) {
	m := registry.Model{
		ModelName: "reasoner", Provider: "deepseek", Model: "deepseek-reasoner",
		APIBase: "https://api.deepseek.com/v1", ThinkingLevel: "high",
	}

	// picoclaw, through materializeModels' entry builder.
	if got := modelListEntry(m)["thinking_level"]; got != "high" {
		t.Errorf("picoclaw model_list entry: thinking_level = %v, want high", got)
	}

	// ganglion, through its own document.
	b, err := ganglionConfigDoc(registry.Resolution{Primary: m}, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	entry := doc["model_list"].([]any)[0].(map[string]any)
	if entry["thinking_level"] != "high" {
		t.Errorf("ganglion model_list entry: thinking_level = %v, want high", entry["thinking_level"])
	}
}

// Omitted, not written empty. For the harness the two differ in more than
// style: an absent key means "the operator said nothing", and an empty one
// would look like they chose to send no depth field on purpose.
func TestAModelWithNoLevelWritesNoKey(t *testing.T) {
	m := registry.Model{
		ModelName: "plain", Provider: "openai", Model: "gpt-5.4",
		APIBase: "https://api.openai.com/v1",
	}
	if _, ok := modelListEntry(m)["thinking_level"]; ok {
		t.Error("picoclaw entry carries an empty thinking_level")
	}
	b, _ := ganglionConfigDoc(registry.Resolution{Primary: m}, nil, "", "")
	var doc map[string]any
	_ = json.Unmarshal(b, &doc)
	if _, ok := doc["model_list"].([]any)[0].(map[string]any)["thinking_level"]; ok {
		t.Error("ganglion entry carries an empty thinking_level")
	}
}
