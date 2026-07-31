package docker

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Dotted-path access over a parsed config.json, for the bulk key editor
// (admin-bulk-instance-config).
//
// valueAtPath (instance_config.go) answers a narrower question — "did this
// managed path change" — and returns nil for a missing segment, for a segment
// that is not an object, and for a real JSON null alike. The bulk editor has to
// tell those apart: a key an instance does not have and a key an instance set to
// null are different buckets in the inspect view, and a non-object in the way is
// one instance's failure rather than a value. So this is a sibling, and
// valueAtPath is left as it is.

// pathState is why a lookup ended where it did.
type pathState int

const (
	// pathFound: the path resolved. The value may be nil, which means JSON null —
	// that is a value, not an absence.
	pathFound pathState = iota
	// pathAbsent: a segment is missing.
	pathAbsent
	// pathConflict: a segment that had to be traversed holds a non-object.
	pathConflict
)

var (
	// ErrPathConflict means a segment on the way to the leaf holds a non-object,
	// so setting the leaf would mean replacing that value instead.
	ErrPathConflict = errors.New("config path traverses a non-object")
	// ErrInvalidConfigKey means the dotted key is unusable as a path or as part of
	// a migration-record filename.
	ErrInvalidConfigKey = errors.New("invalid config key")
	// ErrManagedConfigPath means the key names something the proxy owns and
	// rewrites, so an admin edit there could not survive.
	ErrManagedConfigPath = errors.New("config path is managed by the proxy")
)

// configKeySegmentRe is deliberately stricter than JSON permits: a validated key
// becomes part of a migration-record filename, so no segment may carry a path
// separator or a traversal. Dots are separators here, never part of a segment.
var configKeySegmentRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// lookupPath walks a dotted path, distinguishing the three outcomes above.
func lookupPath(doc map[string]any, dotted string) (any, pathState) {
	segs := strings.Split(dotted, ".")
	var cur any = doc
	for i, seg := range segs {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, pathConflict
		}
		v, ok := obj[seg]
		if !ok {
			return nil, pathAbsent
		}
		if i == len(segs)-1 {
			return v, pathFound
		}
		cur = v
	}
	return nil, pathAbsent
}

// setPath sets the leaf at a dotted path, creating only the objects it needs on
// the way. It never replaces a parent wholesale: for tools.web that would drop
// the six sibling providers, and the admin asked to set one leaf.
func setPath(doc map[string]any, dotted string, value any) error {
	segs := strings.Split(dotted, ".")
	cur := doc
	for _, seg := range segs[:len(segs)-1] {
		existing, present := cur[seg]
		if !present {
			cur = childMap(cur, seg)
			continue
		}
		next, ok := existing.(map[string]any)
		if !ok {
			return fmt.Errorf("%w: %q blocked at %q", ErrPathConflict, dotted, seg)
		}
		cur = next
	}
	cur[segs[len(segs)-1]] = value
	return nil
}

// ValidateConfigKey enforces the charset and rejects an empty key or any empty
// segment, so "", ".", "a..b", "a." and ".a" are all refused.
func ValidateConfigKey(dotted string) error {
	if dotted == "" {
		return fmt.Errorf("%w: key must not be empty", ErrInvalidConfigKey)
	}
	for _, seg := range strings.Split(dotted, ".") {
		if !configKeySegmentRe.MatchString(seg) {
			return fmt.Errorf("%w: %q (each dotted segment must match %s)",
				ErrInvalidConfigKey, dotted, configKeySegmentRe)
		}
	}
	return nil
}

// IsManagedConfigPath reports whether a key collides with ManagedConfigPaths in
// any of three ways, all of which make an admin edit pointless or destructive:
//
//	equal      model_list                        — the proxy replaces it outright
//	under      model_list.deepseek-chat.api_keys — a listed path covers its subtree
//	prefix of  agents, agents.defaults           — setting it replaces the object
//	                                               holding agents.defaults.provider
//
// The prefix relation is the one a leaf-only rule does not cover on its own:
// nothing stops an admin typing an interior path.
//
// Comparison is on SEGMENT boundaries, so model_listing and channel_listx.pico
// are free — a plain string prefix would wrongly call them managed.
func IsManagedConfigPath(dotted string) bool {
	for _, managed := range ManagedConfigPaths {
		if dotted == managed ||
			strings.HasPrefix(dotted, managed+".") ||
			strings.HasPrefix(managed, dotted+".") {
			return true
		}
	}
	return false
}
