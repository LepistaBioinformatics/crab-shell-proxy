package identity

import (
	"path/filepath"
	"strings"
	"testing"
)

// SanitizeID's output names directories: user workspaces, effective-secrets
// dirs, restart markers, project workspaces. Every one of those is built as
// filepath.Join(<root>, SanitizeID(x)), so "the result is one segment and cannot
// leave the root" is a property the whole layout rests on.
//
// It held before this test existed, but only as a consequence of a substitution
// whose stated job is making ids Docker-safe. These assertions state the property
// in its own right, so a future edit to that substitution fails here rather than
// in production.

// hostile is the corpus every assertion below runs over: the shapes an attacker
// would try, plus the degenerate ones a real id can genuinely be.
var hostile = []string{
	"", " ", "\t\n",
	".", "..", "...", "....",
	"/", "//", `\`, `\\`,
	"/etc/passwd", "../../etc/passwd", "a/../b", `a\..\b`,
	strings.Repeat("../", 64),
	"%2e%2e%2f", "..%2f", "..;/",
	"\x00", "a\x00b", "a\nb", "a\rb",
	"~", "~root", "$HOME", "${HOME}", "`id`", "$(id)",
	"con", "nul", "COM1",
	"🌱", "项目一", "Ω≈ç√∫", "…",
	"-leading", "_leading", ".leading", "trailing-", "trailing.",
	"café", "‮exe.txt",
	strings.Repeat("a", 4096),
	"550e8400-e29b-41d4-a716-446655440000", // the normal case: a UUID
}

func TestSanitizeIDAlwaysYieldsOneSegment(t *testing.T) {
	for _, in := range hostile {
		got := SanitizeID(in)
		if got == "" {
			t.Errorf("SanitizeID(%q) = \"\" — an empty segment joins to the root itself", in)
			continue
		}
		if strings.ContainsAny(got, `/\`) {
			t.Errorf("SanitizeID(%q) = %q — contains a path separator", in, got)
		}
		if got == "." || got == ".." {
			t.Errorf("SanitizeID(%q) = %q — names a directory other than itself", in, got)
		}
		if strings.ContainsRune(got, 0) {
			t.Errorf("SanitizeID(%q) = %q — contains NUL", in, got)
		}
		// filepath.Base is the independent statement of "one segment": a value
		// that survives it unchanged has no directory part, whatever it contains.
		if filepath.Base(got) != got {
			t.Errorf("SanitizeID(%q) = %q — filepath.Base disagrees (%q)", in, got, filepath.Base(got))
		}
	}
}

// The assertion that matters, phrased as the consequence rather than the shape:
// joining the result onto a root must land under that root.
func TestSanitizeIDCannotEscapeARoot(t *testing.T) {
	const root = "/data/tenants"
	for _, in := range hostile {
		full := filepath.Clean(filepath.Join(root, SanitizeID(in)))
		if !strings.HasPrefix(full, root+string(filepath.Separator)) {
			t.Errorf("SanitizeID(%q) escapes: %s", in, full)
		}
		// One level deeper, the way the real callers nest
		// (<root>/<tenant>/subscriptions/<subs>/...): a value safe at one level
		// must stay safe when it is not the last component.
		nested := filepath.Clean(filepath.Join(root, SanitizeID(in), "shared", "secrets"))
		if !strings.HasPrefix(nested, root+string(filepath.Separator)) {
			t.Errorf("SanitizeID(%q) escapes when nested: %s", in, nested)
		}
	}
}

// The guard added alongside this test is unreachable for the current pipeline —
// that is what makes it safe to introduce into a system whose directory names
// already exist on disk, since the output cannot change and no workspace is
// orphaned.
//
// This asserts that. If it starts failing, the substitution upstream has changed
// and SanitizeID is now REWRITING ids it used to pass through: the guard has
// become load-bearing, and every existing directory named by the old output has
// just been orphaned. That is a migration, not a patch.
func TestSanitizeIDGuardIsStillUnreachable(t *testing.T) {
	for _, in := range hostile {
		s := unsafeName.ReplaceAllString(strings.TrimSpace(in), "-")
		s = strings.Trim(s, "-._")
		if s == "" {
			continue // the empty case legitimately takes the hash fallback
		}
		if !pathSegment.MatchString(s) {
			t.Errorf("SanitizeID(%q): pre-guard value %q no longer matches the segment "+
				"allowlist — output has changed for existing ids", in, s)
		}
	}
}
