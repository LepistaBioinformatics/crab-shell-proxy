package docker

import (
	"errors"
	"testing"
)

// A key whose value is JSON null must be FOUND, not absent. The two are
// different answers to "does this instance hold a value here", and the bulk
// inspect groups them into different buckets — a null is a value an admin set.
func TestLookupPathSeparatesNullFromAbsent(t *testing.T) {
	doc := map[string]any{
		"tools": map[string]any{
			"web": map[string]any{"brave": nil},
		},
	}

	v, st := lookupPath(doc, "tools.web.brave")
	if st != pathFound {
		t.Errorf("tools.web.brave state = %v, want pathFound", st)
	}
	if v != nil {
		t.Errorf("tools.web.brave = %#v, want nil (JSON null)", v)
	}

	if _, st := lookupPath(doc, "tools.web.tavily"); st != pathAbsent {
		t.Errorf("missing key state = %v, want pathAbsent", st)
	}
	if _, st := lookupPath(doc, "session.dimensions"); st != pathAbsent {
		t.Errorf("missing branch state = %v, want pathAbsent", st)
	}
}

func TestLookupPathReportsConflictOnNonObjectSegment(t *testing.T) {
	doc := map[string]any{"tools": map[string]any{"web": "not-an-object"}}

	if _, st := lookupPath(doc, "tools.web.brave"); st != pathConflict {
		t.Errorf("traversing a string segment = %v, want pathConflict", st)
	}
	// The segment itself still resolves; only going THROUGH it conflicts.
	if v, st := lookupPath(doc, "tools.web"); st != pathFound || v != "not-an-object" {
		t.Errorf("tools.web = %#v/%v, want the string and pathFound", v, st)
	}
}

func TestLookupPathFindsRootAndNestedValues(t *testing.T) {
	doc := map[string]any{
		"version": float64(3),
		"agents":  map[string]any{"defaults": map[string]any{"max_tokens": float64(32768)}},
	}
	if v, st := lookupPath(doc, "version"); st != pathFound || v != float64(3) {
		t.Errorf("version = %#v/%v", v, st)
	}
	if v, st := lookupPath(doc, "agents.defaults.max_tokens"); st != pathFound || v != float64(32768) {
		t.Errorf("agents.defaults.max_tokens = %#v/%v", v, st)
	}
}

// setPath creates the branch it needs and leaves everything else where it was.
// A bulk edit that replaced a parent map would silently drop every sibling key
// in it — for tools.web that is six other providers.
func TestSetPathCreatesBranchAndKeepsSiblings(t *testing.T) {
	doc := map[string]any{
		"tools": map[string]any{
			"web": map[string]any{
				"sogou":  map[string]any{"enabled": true},
				"tavily": map[string]any{"enabled": false},
			},
		},
	}
	if err := setPath(doc, "tools.web.brave.enabled", true); err != nil {
		t.Fatal(err)
	}
	if err := setPath(doc, "heartbeat.interval", float64(30)); err != nil {
		t.Fatal(err)
	}

	web := doc["tools"].(map[string]any)["web"].(map[string]any)
	if brave, ok := web["brave"].(map[string]any); !ok || brave["enabled"] != true {
		t.Errorf("tools.web.brave = %#v, want a block with enabled=true", web["brave"])
	}
	if _, ok := web["sogou"]; !ok {
		t.Error("tools.web.sogou was dropped")
	}
	if _, ok := web["tavily"]; !ok {
		t.Error("tools.web.tavily was dropped")
	}
	if hb, ok := doc["heartbeat"].(map[string]any); !ok || hb["interval"] != float64(30) {
		t.Errorf("heartbeat = %#v, want a created block", doc["heartbeat"])
	}
}

func TestSetPathOverwritesAnExistingLeaf(t *testing.T) {
	doc := map[string]any{"agents": map[string]any{"defaults": map[string]any{"max_tokens": float64(32768)}}}
	if err := setPath(doc, "agents.defaults.max_tokens", float64(65536)); err != nil {
		t.Fatal(err)
	}
	got := doc["agents"].(map[string]any)["defaults"].(map[string]any)["max_tokens"]
	if got != float64(65536) {
		t.Errorf("max_tokens = %#v, want 65536", got)
	}
}

// A non-object in the way is refused rather than replaced: the admin asked to set
// a leaf, and clobbering whatever holds that segment is not what they asked for.
func TestSetPathRefusesToReplaceANonObject(t *testing.T) {
	doc := map[string]any{"tools": map[string]any{"web": "not-an-object"}}
	err := setPath(doc, "tools.web.brave.enabled", true)
	if !errors.Is(err, ErrPathConflict) {
		t.Fatalf("setPath through a string = %v, want ErrPathConflict", err)
	}
	if doc["tools"].(map[string]any)["web"] != "not-an-object" {
		t.Error("the conflicting value was overwritten anyway")
	}
}

func TestValidateConfigKey(t *testing.T) {
	for _, k := range []string{
		"version",
		"tools.web.brave.enabled",
		"agents.defaults.max_tokens",
		"a-b_c.d1",
	} {
		if err := ValidateConfigKey(k); err != nil {
			t.Errorf("ValidateConfigKey(%q) = %v, want nil", k, err)
		}
	}
	// The key becomes part of a migration-record FILENAME, so the charset is
	// stricter than JSON permits: a separator or a traversal must never reach a
	// path join.
	for _, k := range []string{
		"", ".", "..", "a..b", "a.", ".a", "a/b", "a\\b", "a b", "tools.web/brave",
		"a.b..c", "..a",
	} {
		if err := ValidateConfigKey(k); !errors.Is(err, ErrInvalidConfigKey) {
			t.Errorf("ValidateConfigKey(%q) = %v, want ErrInvalidConfigKey", k, err)
		}
	}
}

// All THREE relations to a managed path are refused. The prefix relation is the
// one a leaf-only rule does not cover on its own: setting `agents` replaces the
// object that holds agents.defaults.provider.
func TestIsManagedConfigPathCoversAllThreeRelations(t *testing.T) {
	managed := []string{
		"model_list",                        // equal
		"agents.defaults.provider",          // equal
		"model_list.deepseek-chat",          // under
		"model_list.deepseek-chat.api_keys", // under
		"agents.defaults.workspace",         // equal
		"agents",                            // prefix of agents.defaults.provider
		"agents.defaults",                   // prefix
		"channel_list",                      // prefix of channel_list.pico.enabled
		"channel_list.pico",                 // prefix
	}
	for _, k := range managed {
		if !IsManagedConfigPath(k) {
			t.Errorf("IsManagedConfigPath(%q) = false, want true", k)
		}
	}

	free := []string{
		"version",
		"tools.web.brave.enabled",
		"agents.defaults.max_tokens", // a sibling of a managed leaf is free
		"heartbeat.interval",
		"session.dimensions",
		// Segment boundaries matter: these are NOT related to any managed path,
		// and a naive strings.HasPrefix would call the first two managed.
		"model_listing",
		"agents_extra.defaults",
		"channel_listx.pico",
	}
	for _, k := range free {
		if IsManagedConfigPath(k) {
			t.Errorf("IsManagedConfigPath(%q) = true, want false", k)
		}
	}
}
