package docker

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Per-instance records of one bulk config.json key edit, written so the change
// can be undone later without guessing what the value used to be
// (admin-bulk-instance-config).
//
// These are RECOVERY AIDS, NOT AN AUDIT LOG. The proxy's own log is the record of
// what happened; this is the convenience copy that lives next to the file it
// describes, where whoever is reverting will look for it. A later reader must not
// mistake one artifact for the other.
//
// Unlike config.json and .security.yml, these records are NOT chowned to the
// container user, and the directory is 0700: picoclaw never reads them, so
// granting the agent access would buy nothing and cost the only tamper-resistance
// available here. The agent still owns the parent directory and can therefore
// remove the whole folder — restrict_to_workspace is what actually keeps it out —
// but it cannot read or rewrite an individual record.

// maxMigrationCollisions caps the -2, -3, ... retries. Filenames are second
// resolution because a human reads them, so collisions are real but bounded: one
// bulk edit touches a key once per instance, and each instance has its own
// directory. A double-digit count means something other than a collision.
const maxMigrationCollisions = 9

// migrationStampLayout is second resolution on purpose — deliberately readable
// rather than unique, which is what makes the collision retry necessary.
const migrationStampLayout = "20060102T150405Z"

// ConfigMigration is the before/after of one key on one instance.
type ConfigMigration struct {
	Key string `json:"key"`
	// From is the prior value as raw JSON, so a stored null survives as null.
	From json.RawMessage `json:"from,omitempty"`
	// FromAbsent says the key did not exist before the edit, which "from": null
	// cannot express. Reverting an absent key means DELETING it, not setting it to
	// null, so the two cases must stay distinguishable in the written file: an
	// absent prior value omits "from" entirely and sets this instead.
	FromAbsent     bool                 `json:"fromAbsent,omitempty"`
	To             json.RawMessage      `json:"to"`
	AppliedAt      time.Time            `json:"appliedAt"`
	By             string               `json:"by"`
	Scope          ConfigMigrationScope `json:"scope"`
	RevisionBefore string               `json:"revisionBefore"`
	RevisionAfter  string               `json:"revisionAfter"`
}

// ConfigMigrationScope names the bulk edit the record came from, so a record
// found on its own still says which agent under which subscription it belongs to.
type ConfigMigrationScope struct {
	TenantID  string `json:"tenantId"`
	SubsAccID string `json:"subsAccId"`
	Agent     string `json:"agent"`
}

// writeConfigMigration writes one record into dir and returns the base name it
// chose. rec.Key has already passed ValidateConfigKey at the API edge, so its
// charset is A-Za-z0-9._- — the containedJoin below is the check at the point of
// use, which a future caller that forgets to validate cannot skip.
func writeConfigMigration(dir string, rec ConfigMigration) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	stamp := rec.AppliedAt.UTC().Format(migrationStampLayout)
	body, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return "", err
	}

	for attempt := 1; attempt <= maxMigrationCollisions; attempt++ {
		name := fmt.Sprintf("%s-%s.json", stamp, rec.Key)
		if attempt > 1 {
			name = fmt.Sprintf("%s-%s-%d.json", stamp, rec.Key, attempt)
		}
		// The whole composed name is one path element, so it is the whole thing
		// that has to be checked — joining only the key would leave the suffix
		// unguarded.
		path, err := containedJoin(dir, name)
		if err != nil {
			return "", err
		}
		// O_EXCL is what makes two records in the same second both survive: the
		// loser of the race gets EEXIST and takes the next suffix instead of
		// truncating the record that is already there.
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			if os.IsExist(err) {
				continue
			}
			return "", err
		}
		if _, err := f.Write(body); err != nil {
			f.Close()
			return "", err
		}
		if err := f.Close(); err != nil {
			return "", err
		}
		return name, nil
	}
	return "", fmt.Errorf("config migration record for %q: %d name collisions at %s",
		rec.Key, maxMigrationCollisions, stamp)
}
