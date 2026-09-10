package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

// ganglionMountDest is where the per-user dir is mounted inside the container.
// It matches the harness's own GANGLION_DATA_DIR default.
const ganglionMountDest = "/data/.ganglion"

// ganglionTokenFile holds the per-user bearer, beside the data the harness
// owns. Persisted so a returning user reuses the same token instead of having
// their session invalidated by a recreate.
const ganglionTokenFile = ".crab-ganglion.json"

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
		Binds:   append([]string{hostDir + ":" + ganglionMountDest}, personaBindStrings(m.cfg, key, ganglionMountDest)...),
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

	case personaBindDrift(m.cfg, key, ganglionMountDest, st.Binds) || m.imageDrift(ctx, agent, st):
		// Two drifts, both invisible without this check.
		//
		// Bind sets are fixed at create time, so a container created before a
		// persona file existed has no mount for it and an admin's save can
		// never arrive however many times it is bounced.
		//
		// An image is worse: a container reuses whatever it was created from
		// for as long as it exists, so publishing a new harness image and
		// redeploying the stack changes nothing on its own.
		m.logf("container %s: persona mounts or harness image stale, recreating", name)
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
