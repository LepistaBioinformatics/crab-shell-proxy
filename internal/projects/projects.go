// Package projects holds a user's project list — the "Claude Projects" analogue
// for one (tenant, subscription, agent, user) workspace.
//
// A project is a picoclaw agent of its own: its own workspace, its own AGENT.md,
// inheriting the parent's model, skills and credentials. This package owns only
// the RECORD. Turning records into picoclaw's agents.list / agents.dispatch is
// internal/docker's job, and it re-derives them from here on every ensure —
// materializeModels rewrites the whole config.json, so anything written straight
// into it would be erased on the user's next chat.
//
// The store lives at config.ProjectsFile, deliberately above workspace/: with
// restrict_to_workspace the agent cannot reach it. An agent able to edit this
// file could invent agent identities and dispatch rules, which is a routing
// decision, not agent business.
package projects

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Project is one project record. Instructions is user-authored text that becomes
// the body of the project workspace's AGENT.md.
type Project struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Instructions string    `json:"instructions,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
}

var (
	ErrNotFound   = errors.New("project not found")
	ErrDuplicate  = errors.New("a project with that name already exists")
	ErrEmptyName  = errors.New("project name must not be empty")
	ErrReservedID = errors.New("project id is reserved")
)

// MaxIDLength mirrors picoclaw's routing.MaxAgentIDLength. An ID longer than
// this would be TRUNCATED by NormalizeAgentID inside picoclaw, and a truncated
// id no longer matches the dispatch rule the proxy wrote — the project would
// silently stop routing. So the limit is enforced here instead.
const MaxIDLength = 64

// DefaultAgentID is picoclaw's implicit main agent. A project may not claim it:
// the projection always emits {id: "main", default: true}, and a second entry
// with that id would collide with it.
const DefaultAgentID = "main"

// validID mirrors picoclaw's routing.validIDRe exactly. Generated IDs must be
// fixed points of NormalizeAgentID — if picoclaw rewrites the id, the agent it
// registers and the id in our dispatch rule stop agreeing.
//
// Note what the alphabet EXCLUDES, both load-bearing:
//   - "*" cannot appear, so a project name can never inject a wildcard into its
//     own dispatch pattern.
//   - "." cannot appear, which is what makes it a safe separator in the
//     "p.<id>.<key>" session id. With "-" as the separator, projects "my" and
//     "my-proj" would both be matched by the pattern "p-my-*".
var validID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

var nonIDChars = regexp.MustCompile(`[^a-z0-9_-]+`)

// Store is the on-disk project list for one workspace. Every mutation is a
// read-modify-write of the whole file under a lock: the list is small, and a
// partial write here would corrupt the routing table.
type Store struct {
	path string
	mu   sync.Mutex
}

func NewStore(path string) *Store {
	return &Store{path: path}
}

// List returns the projects in creation order. An absent file is an empty list,
// not an error — a user with no projects is the normal state.
func (s *Store) List() ([]Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

// Get returns one project by id.
func (s *Store) Get(id string) (Project, error) {
	list, err := s.List()
	if err != nil {
		return Project{}, err
	}
	for _, p := range list {
		if p.ID == id {
			return p, nil
		}
	}
	return Project{}, fmt.Errorf("%w: %q", ErrNotFound, id)
}

// Create adds a project, deriving its id from name. createdAt is passed in
// rather than read from the clock so callers can keep the record deterministic
// in tests.
func (s *Store) Create(name, instructions string, createdAt time.Time) (Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	name = strings.TrimSpace(name)
	if name == "" {
		return Project{}, ErrEmptyName
	}

	list, err := s.load()
	if err != nil {
		return Project{}, err
	}
	for _, p := range list {
		if strings.EqualFold(p.Name, name) {
			return Project{}, fmt.Errorf("%w: %q", ErrDuplicate, name)
		}
	}

	taken := make(map[string]bool, len(list)+1)
	taken[DefaultAgentID] = true
	for _, p := range list {
		taken[p.ID] = true
	}
	id, err := generateID(name, taken)
	if err != nil {
		return Project{}, err
	}

	p := Project{ID: id, Name: name, Instructions: instructions, CreatedAt: createdAt.UTC()}
	return p, s.save(append(list, p))
}

// Rename changes the display name only. The id is NOT re-derived: it is baked
// into a dispatch rule, a workspace directory and every session id already
// issued for this project, so re-deriving it would orphan the project's
// conversations and files.
func (s *Store) Rename(id, name string) (Project, error) {
	return s.mutate(id, func(p *Project, others []Project) error {
		name = strings.TrimSpace(name)
		if name == "" {
			return ErrEmptyName
		}
		for _, o := range others {
			if strings.EqualFold(o.Name, name) {
				return fmt.Errorf("%w: %q", ErrDuplicate, name)
			}
		}
		p.Name = name
		return nil
	})
}

// SetInstructions replaces the project's instruction text.
func (s *Store) SetInstructions(id, instructions string) (Project, error) {
	return s.mutate(id, func(p *Project, _ []Project) error {
		p.Instructions = instructions
		return nil
	})
}

// Delete removes the record. Removing the project's WORKSPACE is the caller's
// job — this package does not know where it lives, and the two steps are
// deliberately separable so a failed directory removal cannot leave a record
// pointing at nothing.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	list, err := s.load()
	if err != nil {
		return err
	}
	out := make([]Project, 0, len(list))
	found := false
	for _, p := range list {
		if p.ID == id {
			found = true
			continue
		}
		out = append(out, p)
	}
	if !found {
		return fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	return s.save(out)
}

func (s *Store) mutate(id string, apply func(p *Project, others []Project) error) (Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	list, err := s.load()
	if err != nil {
		return Project{}, err
	}
	idx := -1
	others := make([]Project, 0, len(list))
	for i, p := range list {
		if p.ID == id {
			idx = i
			continue
		}
		others = append(others, list[i])
	}
	if idx < 0 {
		return Project{}, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	if err := apply(&list[idx], others); err != nil {
		return Project{}, err
	}
	return list[idx], s.save(list)
}

func (s *Store) load() ([]Project, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read projects file: %w", err)
	}
	var list []Project
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("parse projects file: %w", err)
	}
	return list, nil
}

// save writes through a temp file in the same directory and renames, so a crash
// mid-write leaves the previous list intact rather than a truncated one. 0600:
// the proxy is the only reader.
func (s *Store) save(list []Project) error {
	if list == nil {
		list = []Project{}
	}
	out, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".projects-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

// generateID derives a picoclaw-safe agent id from a display name, suffixing on
// collision.
//
// A user-supplied id is never accepted anywhere in this feature: the value
// becomes an agent identity, a filesystem path and part of a dispatch pattern,
// and each of those has a different thing it must not contain.
func generateID(name string, taken map[string]bool) (string, error) {
	slug := nonIDChars.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-")
	slug = strings.Trim(slug, "-_")
	// The alphabet allows "_" but not a leading one, and requires an
	// alphanumeric first character.
	slug = strings.TrimLeft(slug, "-_")
	if len(slug) > MaxIDLength {
		slug = strings.TrimRight(slug[:MaxIDLength], "-_")
	}
	if slug == "" || !validID.MatchString(slug) {
		// Names made entirely of characters outside the alphabet (CJK, emoji,
		// punctuation) legitimately reach here. A deterministic fallback is better
		// than rejecting the name: the id is an internal identifier, and the user
		// sees Name.
		slug = "project"
	}

	if !taken[slug] {
		return slug, nil
	}
	for n := 2; n < 1000; n++ {
		suffix := fmt.Sprintf("-%d", n)
		base := slug
		if len(base)+len(suffix) > MaxIDLength {
			base = strings.TrimRight(base[:MaxIDLength-len(suffix)], "-_")
		}
		candidate := base + suffix
		if !taken[candidate] && validID.MatchString(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not derive a unique project id from %q", name)
}
