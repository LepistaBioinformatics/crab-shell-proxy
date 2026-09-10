package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

// crab-ganglion-harness containers.
//
// This is a SEPARATE creation path rather than conditionals threaded through
// create(). Two reasons, and the second is the load-bearing one:
//
//   - The two harnesses share almost nothing at container level. picoclaw needs
//     a workspace laid out under <HOME>/.picoclaw with a config.json, a
//     .security.yml, a persona cascade of read-only file binds, a skills root
//     and per-project secret binds. The ganglion reads its whole configuration
//     from the environment and owns one directory.
//   - picoclaw is what production runs, and internal/docker's suite does not
//     execute in every environment (it needs lchown privileges). Editing the
//     path that serves every live member, without a test that runs, to
//     accommodate a harness nobody is on yet, is the wrong trade. This file
//     cannot regress picoclaw because picoclaw never enters it.
//
// The Hermes work took the same shape (createHermes) for the same reason.

// ganglionMountDest is the harness's data root inside the container, matching
// its own GANGLION_DATA_DIR default. NOT a mount: only its workspace child is
// bound (ganglionWorkspaceBind), and the rest of the per-user dir stays on the
// host where the agent cannot reach it.
const ganglionMountDest = "/data/.ganglion"

// ganglionTokenFile holds the per-user bearer, in the user directory but
// DELIBERATELY OUTSIDE what the container mounts (see ganglionWorkspaceBind).
// Persisted so a returning user reuses the same token instead of having their
// session invalidated by a recreate.
const ganglionTokenFile = ".crab-ganglion.json"

// ganglionWorkspaceBind mounts only the workspace, not the whole user dir.
//
// The wide bind (hostDir -> ganglionMountDest) is what the first cut used, and
// it put .crab-ganglion.json one level above the shell tool's working
// directory: `cat ../.crab-ganglion.json` read the bearer. That slot -- proxy
// owned state beside the workspace -- is exactly the one picoclaw's
// restrict_to_workspace protects, and the ganglion has no equivalent: its only
// tool is `/bin/sh -c <string>`, so there is no path argument to refuse. The
// container boundary IS the confinement here, which means proxy-owned state
// has to be on the other side of it rather than merely adjacent.
//
// Anything the harness needs to keep across a recreate therefore belongs under
// workspace/; anything the proxy owns belongs above it, and is now unreachable
// rather than one `..` away.
func ganglionWorkspaceBind(hostDir string) string {
	return filepath.Join(hostDir, config.MainWorkspace) + ":" + ganglionMountDest + "/" + config.MainWorkspace
}

// ganglionBinds is the container's whole bind set.
//
// Split out of createGanglion for the reason ganglionEnv was: the drift checks
// read this list back and must agree with it, and neither of them can be
// exercised where a Docker daemon is needed. A disagreement here does not fail
// loudly -- it recreates the container on every single turn, which reads as
// "the harness keeps restarting" with nothing naming the cause.
func ganglionBinds(cfg *config.Config, key WorkspaceKey, hostDir string) []string {
	binds := []string{ganglionWorkspaceBind(hostDir)}
	if cfg.GanglionKeyFile != "" {
		binds = append(binds, cfg.GanglionKeyFile+":"+ganglionKeyFileDest+":ro")
	}
	return append(binds, personaBindStrings(cfg, key, ganglionMountDest)...)
}

// ganglionKeyFileDest is where the credential key file lands in the container.
//
// Beside the data root, NOT under workspace/ -- and that placement is the
// whole second factor. The workspace is the only hierarchy the harness's
// Landlock ruleset grants a command, so a file here is unreachable from the
// shell tool while remaining readable by the harness process at boot. Move it
// under workspace/ and the two-factor scheme collapses to one factor plus a
// file the agent can print.
const ganglionKeyFileDest = ganglionMountDest + "/credential.key"

// ganglionBindDrift reports whether an existing container's mounts no longer
// match what createGanglion would build.
//
// Without this a running container keeps the bind set it was created with
// forever, because bind sets are fixed at create time. Two cases, both
// invisible otherwise and both security-relevant:
//
//   - the OLD WIDE BIND, which exposed the whole per-user directory and with
//     it the proxy's own state;
//   - the CREDENTIAL KEY FILE appearing or disappearing, so that turning
//     encryption on reaches members already on the harness instead of leaving
//     them booting against a value they cannot resolve.
//
// Same class of invisible staleness as personaBindDrift and imageDrift, and it
// sits beside them for that reason.
func ganglionBindDrift(cfg *config.Config, actual []string) bool {
	haveKeyFile := false
	for _, b := range actual {
		_, dest, ok := splitBind(b)
		if !ok {
			continue
		}
		if dest == ganglionMountDest {
			return true
		}
		if dest == ganglionKeyFileDest {
			haveKeyFile = true
		}
	}
	return haveKeyFile != (cfg.GanglionKeyFile != "")
}

// splitBind pulls the destination out of a "src:dest[:opts]" bind string.
func splitBind(b string) (src, dest string, ok bool) {
	parts := strings.Split(b, ":")
	if len(parts) < 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

type ganglionState struct {
	Token string `json:"token"`
}

// provisionGanglion prepares the per-user directory and returns the bearer the
// proxy will present to that container.
//
// Unlike picoclaw's provision it writes no config: everything the harness needs
// arrives as environment, which is why the generic dotenv secret sink covers it
// and the picoclaw-only native .security.yml sink is simply unused.
func provisionGanglion(userDir, user string) (string, error) {
	if err := os.MkdirAll(filepath.Join(userDir, "workspace"), 0o755); err != nil {
		return "", fmt.Errorf("ganglion workspace: %w", err)
	}
	statePath := filepath.Join(userDir, ganglionTokenFile)

	if b, err := os.ReadFile(statePath); err == nil {
		var st ganglionState
		if json.Unmarshal(b, &st) == nil && st.Token != "" {
			return st.Token, nil
		}
		// A corrupt or empty state file is replaced rather than fatal: the cost
		// is one invalidated bearer, and the alternative is a member who cannot
		// start a container until someone deletes a file by hand.
	}

	tok, err := randomToken()
	if err != nil {
		return "", fmt.Errorf("ganglion token: %w", err)
	}
	b, err := json.Marshal(ganglionState{Token: tok})
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(statePath, b, 0o600); err != nil {
		return "", fmt.Errorf("write ganglion state: %w", err)
	}
	if err := chownTree(userDir, user); err != nil {
		return "", fmt.Errorf("chown ganglion dir: %w", err)
	}
	return tok, nil
}

// ganglionEnv builds the container environment.
//
// Split out from createGanglion so it is assertable without a Docker daemon --
// which matters more here than usual, because this is the only place the
// harness's configuration contract is expressed and a typo in a variable name
// fails as "the harness would not boot", with nothing naming the cause.
func ganglionEnv(cfg *config.Config, agent config.Agent, token string) []string {
	env := []string{
		fmt.Sprintf("GANGLION_ADDR=0.0.0.0:%d", cfg.GanglionPort),
		"GANGLION_DATA_DIR=" + ganglionMountDest,
		"GANGLION_TOKEN=" + token,
	}
	if agent.Model != nil {
		env = append(env,
			"GANGLION_MODEL="+agent.Model.Name,
			"GANGLION_BASE_URL="+agent.Model.BaseURL,
			"GANGLION_API_KEY="+agent.Model.APIKey,
		)
	}
	if cfg.GanglionOTLPEndpoint != "" {
		env = append(env, "GANGLION_OTLP_ENDPOINT="+cfg.GanglionOTLPEndpoint)
	}
	// The other half of an enc:// credential. The ciphertext travels in
	// GANGLION_API_KEY above, already opaque to this proxy; this is the factor
	// that is useless on its own without the bound key file.
	if cfg.GanglionKeyPassphrase != "" {
		env = append(env, "GANGLION_KEY_PASSPHRASE="+cfg.GanglionKeyPassphrase)
		env = append(env, "GANGLION_KEY_FILE="+ganglionKeyFileDest)
	}
	// The persona file the proxy's cascade already materializes. Read at every
	// turn rather than at boot, so an admin's edit reaches the member without a
	// container restart.
	env = append(env, "GANGLION_SYSTEM_FILE="+ganglionMountDest+"/workspace/AGENT.md")
	return env
}

// createGanglion creates (but does not start) a ganglion container.
func (m *Manager) createGanglion(ctx context.Context, agent config.Agent, key WorkspaceKey, name string) error {
	hostDir := config.UserWorkspace(m.cfg.HostDataRoot, key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	containerDir := config.UserWorkspace(m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role, key.UserAccID)

	token, err := provisionGanglion(containerDir, m.cfg.PicoclawUser)
	if err != nil {
		return err
	}

	image := m.harnessImage(agent)
	if image == "" {
		// Unreachable through config.Load, which rejects such an agent. Kept so
		// a programmatic Config cannot create a container from an empty image
		// name and fail as a confusing daemon error.
		return fmt.Errorf("agent %q: no ganglion image configured", agent.Key)
	}

	spec := CreateSpec{
		Name:  name,
		Image: image,
		// Same uid the proxy already chowns per-user volumes to. Unlike Hermes,
		// there is no s6 setuidgid step to break by setting this.
		User: m.cfg.PicoclawUser,
		Env:  ganglionEnv(m.cfg, agent, token),
		Labels: map[string]string{
			LabelManaged:      "true",
			LabelAgent:        key.Role,
			LabelTenant:       key.TenantID,
			LabelSubscription: key.SubsAccID,
			LabelUser:         key.UserAccID,
			LabelMode:         string(agent.Mode),
		},
		// The per-user volume, plus the persona cascade READ-ONLY on top of it.
		//
		// personaBinds is harness-agnostic -- it takes the mount destination --
		// and it lands each file at <mountDest>/workspace/<name>, which is
		// exactly where GANGLION_SYSTEM_FILE points. Without this the harness
		// logged "persona file ... no such file or directory" every boot and
		// ran every member's agent with no identity at all: the env var was
		// wired to a path nothing created.
		Binds:   ganglionBinds(m.cfg, key, hostDir),
		Network: m.cfg.Network,
		// One process, PID 1, signals handled in main. No supervisor to reap
		// children for.
		Init: false,
	}
	if err := m.docker.EnsureImage(ctx, image); err != nil {
		return fmt.Errorf("ensure image %s: %w", image, err)
	}
	if _, err := m.docker.Create(ctx, spec); err != nil {
		return err
	}
	m.logf("created ganglion container %s (mode=%s)", name, agent.Mode)
	return nil
}

// ensureGanglionRunning brings a ganglion container to health-ready and
// returns how to reach it.
//
// It is the tail of EnsureRunning for this harness, and it is short for the
// same reason ganglion.go exists at all: none of what makes the picoclaw tail
// long — the persona bind cascade, per-project secret binds, the skills root,
// the managed-content mounts — has a ganglion equivalent yet. When one does, it
// belongs here rather than as another branch in the picoclaw path.
func (m *Manager) ensureGanglionRunning(
	ctx context.Context, agent config.Agent, key WorkspaceKey, name, authToken string,
) (Target, error) {
	st, err := m.docker.Inspect(ctx, name)
	if err != nil {
		return Target{}, err // daemon unreachable etc. — surfaced as 502 upstream
	}
	switch {
	case !st.Exists:
		if cerr := m.createGanglion(ctx, agent, key, name); cerr != nil {
			return Target{}, cerr
		}
		if serr := m.docker.Start(ctx, name); serr != nil {
			return Target{}, serr
		}

	case personaBindDrift(m.cfg, key, ganglionMountDest, st.Binds) ||
		ganglionBindDrift(m.cfg, st.Binds) ||
		m.imageDrift(ctx, agent, st):
		// Three drifts, all invisible without this check.
		//
		// Bind sets are fixed at create time, so a container created before a
		// persona file existed has no mount for it and an admin's save can
		// never arrive however many times it is bounced.
		//
		// An image is worse: a container reuses whatever it was created from
		// for as long as it exists, so publishing a new harness image and
		// redeploying the stack changes nothing on its own.
		//
		// The third is the bind set: a container created before the mount was
		// narrowed still exposes the proxy's own state to the agent, and one
		// created before encryption was switched on has no key file to
		// resolve its own credential with.
		m.logf("container %s: persona mounts, bind set or harness image stale, recreating", name)
		if st.Running {
			if serr := m.docker.Stop(ctx, name, 10*time.Second); serr != nil {
				return Target{}, serr
			}
		}
		if rerr := m.docker.Remove(ctx, name); rerr != nil {
			return Target{}, rerr
		}
		if cerr := m.createGanglion(ctx, agent, key, name); cerr != nil {
			return Target{}, cerr
		}
		if serr := m.docker.Start(ctx, name); serr != nil {
			return Target{}, serr
		}

	case !st.Running:
		if serr := m.docker.Start(ctx, name); serr != nil {
			return Target{}, serr
		}
	}

	port := m.harnessPort(agent)
	if err := m.waitHealthy(ctx, name, port, m.startupDeadline(agent)); err != nil {
		return Target{}, fmt.Errorf("%s %s did not become ready: %w", agent.Harness, name, err)
	}
	// The idle timer is NOT armed here. EnsureRunning already holds this
	// container's keyState lock and ArmIdle takes it again -- arming from
	// inside would deadlock. The picoclaw path does not arm here either: the
	// caller re-arms after the turn, which is also the only moment "idle" is
	// true.
	return Target{
		Name:      name,
		Endpoint:  m.endpoint(agent, name),
		AuthToken: authToken,
		Harness:   agent.Harness,
	}, nil
}
