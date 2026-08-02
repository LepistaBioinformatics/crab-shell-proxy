package memgraph

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// MaxBytes caps one workspace's encoded graph. A graph is loaded whole into the
// agent's tool result, so an unbounded file degrades every turn long before it
// fills a disk; failing the write that would cross the line is more useful to the
// agent than silently growing.
const MaxBytes = 4 << 20

// ErrTooLarge is returned by Update when the mutated graph would exceed MaxBytes.
// The on-disk file is left untouched.
var ErrTooLarge = errors.New("memory graph exceeds the size limit")

// Store owns the on-disk graphs. One Store serves every workspace; the per-scope
// mutex is what keeps them independent.
type Store struct {
	root string
	now  func() time.Time

	mu    sync.Mutex
	locks map[Scope]*sync.Mutex
}

// NewStore builds a Store rooted at the proxy's view of the data root
// (containerDataRoot, not hostDataRoot — this process reads and writes these
// files itself).
//
// now is injected because two behaviours are otherwise untestable: the timestamps
// stamped onto legacy records, and get_recent_changes' window.
func NewStore(containerDataRoot string, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{
		root:  containerDataRoot,
		now:   now,
		locks: make(map[Scope]*sync.Mutex),
	}
}

// NowMillis is the clock in the unit the stored records use (epoch milliseconds,
// matching upstream's Date.now()).
func (s *Store) NowMillis() int64 { return s.now().UnixMilli() }

// lockFor returns the mutex guarding one workspace's file, creating it on first
// use. Entries are never evicted: one mutex per (member, agent) pair is a handful
// of bytes, and evicting safely would need reference counting for no benefit.
func (s *Store) lockFor(sc Scope) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.locks[sc]
	if !ok {
		l = &sync.Mutex{}
		s.locks[sc] = l
	}
	return l
}

// Load reads one workspace's graph. An absent file is an empty graph, not an
// error — a member who has never stored anything simply has nothing.
//
// Load takes no lock. Writes land by rename, which is atomic, so a concurrent
// Update is observed as either the whole old file or the whole new one and a
// reader can never see a torn graph. Keeping reads lock-free means the UI's read
// endpoints never wait behind an agent's write.
func (s *Store) Load(sc Scope) (*Graph, error) {
	f, err := os.Open(s.Path(sc))
	if err != nil {
		if os.IsNotExist(err) {
			return &Graph{}, nil
		}
		return nil, err
	}
	defer f.Close()
	return decodeJSONL(f, s.NowMillis())
}

// Update is the ONLY writer. It takes the workspace's lock, loads the graph,
// hands it to fn, then writes the result back atomically.
//
// Every mutating operation goes through here, so no operation can forget to lock,
// to bound the result, or to write atomically. An error from fn aborts the write
// and leaves the file exactly as it was.
func (s *Store) Update(sc Scope, fn func(*Graph) error) error {
	l := s.lockFor(sc)
	l.Lock()
	defer l.Unlock()

	g, err := s.Load(sc)
	if err != nil {
		return err
	}
	if err := fn(g); err != nil {
		return err
	}
	out, err := encodeJSONL(g)
	if err != nil {
		return err
	}
	if len(out) > MaxBytes {
		return fmt.Errorf("%w (%d bytes, limit %d)", ErrTooLarge, len(out), MaxBytes)
	}
	return s.writeAtomic(sc, out)
}

// writeAtomic replaces the graph file by temp-file-then-rename, so a crash or a
// full disk mid-write cannot truncate a member's memory: either the old file
// survives whole or the new one does.
//
// The temp file is created in the SAME directory as the target, because rename(2)
// is only atomic within a filesystem.
func (s *Store) writeAtomic(sc Scope, data []byte) error {
	dir := s.Dir(sc)
	// 0700 and root-owned, never chowned to picoclawUser — see GraphDirName.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create memory graph dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".memory-*.jsonl.tmp")
	if err != nil {
		return fmt.Errorf("create memory graph temp file: %w", err)
	}
	tmpName := tmp.Name()
	// Remove the temp file on every path that does not reach the rename. After a
	// successful rename tmpName no longer exists and Remove is a harmless no-op.
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write memory graph: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync memory graph: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close memory graph: %w", err)
	}
	// CreateTemp makes the file 0600, which is what we want, but be explicit: the
	// value is a credential-adjacent record of everything the agent has learned.
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("chmod memory graph: %w", err)
	}
	if err := os.Rename(tmpName, s.Path(sc)); err != nil {
		return fmt.Errorf("replace memory graph: %w", err)
	}
	return nil
}
