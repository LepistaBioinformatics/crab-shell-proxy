package toolaudit

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"testing"
)

// THE CONTRACT WITH THE HARNESS, which no compiler checks.
//
// The ganglion writes these records and this package reads them. Nothing links
// the two: a rename on either side compiles, passes every test in its own
// repository, and then decodes to a zero value forever. The member does not see
// an error -- they see a sheet that says the call was not recorded, which is
// also what a genuinely unrecorded call says. That is the worst shape a bug can
// have, and it is exactly what this file exists to turn back into a failure.
//
// Skipped rather than failed when the harness is not beside this checkout, the
// way TestSecretPrefixMatchesTheHarness is: the proxy is its own repository and
// must build alone.

func harnessSource(t *testing.T) []byte {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(
		"..", "..", "..", "crab-ganglion-harness",
		"internal", "adapter", "store", "toolaudit", "toolaudit.go",
	))
	if err != nil {
		t.Skipf("the harness checkout is not beside this one: %v", err)
	}
	return src
}

func TestDirNameMatchesTheHarness(t *testing.T) {
	src := harnessSource(t)
	m := regexp.MustCompile(`DirName = "([^"]+)"`).FindSubmatch(src)
	if m == nil {
		t.Fatal("no DirName in the harness's toolaudit store: the pattern no longer matches, so this proves nothing")
	}
	// The proxy's own copy of the name lives in config, which this package does
	// not import; the literal is repeated here deliberately so the test fails on
	// a change to either side rather than following one of them.
	if got := string(m[1]); got != ".tool-audit" {
		t.Errorf("the harness writes to %q and the proxy reads %q; every sheet would say 'not recorded'", got, ".tool-audit")
	}
}

// EVERY JSON NAME, not just the two that would be noticed. `arguments` and
// `output` are what the member came to read, so their drift is loud -- but a
// renamed `status` silently turns every call into one with no outcome, which
// reads as an interrupted turn.
func TestRecordFieldNamesMatchTheHarness(t *testing.T) {
	src := harnessSource(t)

	// The harness's Record, from its struct tags.
	block := regexp.MustCompile(`(?s)type Record struct \{(.*?)\n\}`).FindSubmatch(src)
	if block == nil {
		t.Fatal("no Record struct in the harness's toolaudit store: the pattern no longer matches, so this proves nothing")
	}
	theirs := regexp.MustCompile(`json:"([^",]+)`).FindAllSubmatch(block[1], -1)
	if len(theirs) == 0 {
		t.Fatal("the harness's Record carries no json tags: the pattern no longer matches")
	}
	want := make([]string, 0, len(theirs))
	for _, m := range theirs {
		want = append(want, string(m[1]))
	}

	// Ours, from the type itself rather than from our own source: this is the
	// thing that actually decodes, so reading it any other way could agree with
	// the harness while the decoder disagreed with both.
	rt := reflect.TypeOf(Record{})
	got := make([]string, 0, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("json")
		for j := 0; j < len(tag); j++ {
			if tag[j] == ',' {
				tag = tag[:j]
				break
			}
		}
		got = append(got, tag)
	}

	sort.Strings(want)
	sort.Strings(got)
	if !reflect.DeepEqual(want, got) {
		t.Errorf("the record's fields have drifted.\nharness writes: %v\nproxy reads:    %v", want, got)
	}
}
