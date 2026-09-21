package docker

import (
	"fmt"
	"strings"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
)

// SignedInFileName is the document naming the account this workspace is running
// for. It lives in the workspace's memory directory, which both harnesses read
// in full on every turn -- the ganglion's skills.Prompt enumerates every `.md`
// there rather than naming files, and picoclaw does the same. So this reaches
// the system prompt with no change on either side.
const SignedInFileName = "SIGNED_IN_USER.md"

// signedInRel is its path relative to the workspace root. Two fixed components,
// no caller input in either -- which is exactly what memoryRel's comment warns
// is not by itself a reason to write it unconfined.
const signedInRel = MemoryDirName + "/" + SignedInFileName

// Owner is the signed-in account a workspace is being ensured for.
//
// A struct rather than the bare email this parameter used to be, because the
// email was never the thing that was missing: the proxy has resolved the member's
// NAME on every turn since identity decoding existed, and dropped it one frame
// below the handler.
//
// Every field is optional. A scheduled turn carries an email and no profile, and
// a reconcile carries nothing at all.
type Owner struct {
	Email     string
	FirstName string
	LastName  string
	Username  string
}

// Name is the person's name, or "" when the profile carried none.
func (o Owner) Name() string {
	return strings.TrimSpace(strings.TrimSpace(o.FirstName) + " " + strings.TrimSpace(o.LastName))
}

// Empty reports whether there is nothing to say about this owner.
//
// Exported because the CALLER has to be able to tell "nothing to record" from
// "recorded": both leave the workspace unchanged, and only one of them is a
// reconcile doing the right thing.
func (o Owner) Empty() bool {
	return o.Name() == "" && strings.TrimSpace(o.Username) == "" && strings.TrimSpace(o.Email) == ""
}

// SeedSignedInUser writes the signed-in account into the MAIN workspace's memory.
//
// The main workspace only. skills.Prompt reads it on every turn INCLUDING a
// project's -- that is stated in workspaceSections' own comment -- so one file
// covers every conversation, and a copy per project would be N files to keep in
// agreement for no added reach.
//
// An owner with nothing in it is not written. A reconcile restarts containers
// with no caller and therefore no identity (reconcile.go), and rendering a
// document from that would replace a correct file with an empty one at every
// restart. The symptom would be the agent forgetting who it is talking to, which
// is the bug this exists to fix.
//
// Confined like WriteMemory, and for the reason WriteMemory documents: the proxy
// runs as root and `workspace/memory` is owned by the agent's uid, so a plain
// WriteFile there follows whatever the agent may have planted as `memory`.
func (m *Manager) SeedSignedInUser(key WorkspaceKey, harness string, o Owner) error {
	if o.Empty() {
		return nil
	}
	tree, err := openTree(m.workspaceDir(key, harness, ""))
	if err != nil {
		return err
	}
	defer tree.Close()

	if err := tree.root.MkdirAll(MemoryDirName, 0o700); err != nil {
		if escaped(err) {
			return ErrMediaName
		}
		return fmt.Errorf("mkdir memory: %w", err)
	}
	if err := tree.root.WriteFile(signedInRel, []byte(signedInDoc(o)), 0o600); err != nil {
		if escaped(err) {
			return ErrMediaName
		}
		return fmt.Errorf("write %s: %w", SignedInFileName, err)
	}
	if err := chownTree(tree.abs(MemoryDirName), m.cfg.PicoclawUser); err != nil {
		return fmt.Errorf("chown memory: %w", err)
	}
	return nil
}

// signedInDoc renders the account as the Markdown the harness folds into the
// prompt.
//
// A MISSING FIELD IS OMITTED, never rendered as a placeholder. An agent given
// `Name: (unknown)` says "(unknown)" back to the member; an agent given no name
// asks for one. Nothing here is inferred either -- no timezone from a locale, no
// pronouns from a name -- because the member has no way to see where an invented
// fact came from.
//
// It also says what it is and which file the agent should write instead. The two
// sit in the same directory and one of them is the agent's own; without the
// pointer the obvious place to record something learned about the member is the
// file that already describes them, and the next turn would overwrite it.
func signedInDoc(o Owner) string {
	var b strings.Builder
	b.WriteString("# Who you are talking to\n\n")
	for _, f := range []struct{ label, value string }{
		{"Name", o.Name()},
		{"Username", strings.TrimSpace(o.Username)},
		{"E-mail", strings.TrimSpace(o.Email)},
	} {
		if f.value != "" {
			fmt.Fprintf(&b, "- %s: %s\n", f.label, f.value)
		}
	}
	b.WriteString("\nThis is the signed-in account, written by the platform before each turn. " +
		"It is not yours to edit: the next turn replaces it, and the account itself is " +
		"where a correction has to be made.\n\n" +
		"What you LEARN about this person -- how they prefer to work, what they are " +
		"working on -- belongs in `memory/USER.md`, which is yours to write.\n")
	return b.String()
}

// OwnerFrom reads the signed-in account off a resolved identity.
//
// Here rather than in the handlers because all three turn paths need the same
// mapping, and a second copy of it is how one of them comes to send a different
// name than the others.
func OwnerFrom(ident identity.Identity) Owner {
	o := Owner{Email: ident.Email}
	p, ok := ident.PrincipalOwner()
	if !ok {
		return o
	}
	o.FirstName = deref(p.FirstName)
	o.LastName = deref(p.LastName)
	o.Username = deref(p.Username)
	return o
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
