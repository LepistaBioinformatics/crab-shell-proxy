package history

import (
	"errors"
	"io/fs"
	"os"
	"strings"
)

// Transcripts live inside the agent's own workspace — <workspace>/sessions —
// which is bind-mounted read-write into its container and chowned to the uid the
// agent runs as. picoclaw writes its session files there, which is the point;
// the consequence is that every component of a transcript path is under the
// agent's control, and this package reads and appends to those paths AS ROOT.
//
// Two things that buys an agent able to plant a symlink, before this file:
//
//   - Read followed `durable/<key>.jsonl` wherever it pointed and returned the
//     contents to the member as their own conversation — an arbitrary-file read,
//     rendered in the chat transcript.
//   - SyncDurable opened the same path O_CREATE|O_APPEND with mode 0644 and
//     appended message text to it. A root-owned append primitive at a
//     agent-chosen path.
//
// Neither is reachable through the session id: that is hashed to hex before it
// gets here. The path is attacker-controlled through the DIRECTORY, not the name
// — which is why input validation never had anything to say about it.
//
// openSessions confines all of it to sessionsDir. A missing directory is the
// normal state for a conversation picoclaw has not persisted yet, so it is
// reported as such rather than as a failure.

var errNoSessionsDir = errors.New("history: no sessions directory")

func openSessions(sessionsDir string) (*os.Root, error) {
	r, err := os.OpenRoot(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errNoSessionsDir
		}
		return nil, err
	}
	return r, nil
}

// escapes reports whether err is the kernel refusing to leave the root, as
// opposed to a genuine I/O failure. os keeps the sentinel unexported; see
// TestEscapesRecognisesOnlyTheRefusal for what pins this to the real wording.
const pathEscapesMsg = "path escapes from parent"

func escapes(err error) bool {
	var pe *fs.PathError
	return errors.As(err, &pe) && pe.Err != nil &&
		strings.Contains(pe.Err.Error(), pathEscapesMsg)
}

// existsIn reports whether rel names something inside the root. A path that
// would leave it is "does not exist" as far as this package is concerned: there
// is no transcript there, whatever the link points at.
func existsIn(r *os.Root, rel string) bool {
	_, err := r.Stat(rel)
	return err == nil
}
