//go:build integration

// Integration test for the raw-HTTP Docker Engine API client. Requires a real
// Docker daemon socket; run with:
//
//	go test -tags integration ./internal/docker -run TestIntegration -v
//
// It uses a throwaway alpine container (no picoclaw / LLM keys needed) to prove
// the hand-written request encoding actually round-trips through the daemon:
// EnsureImage → Create (bind + network + labels + cmd) → Start → Inspect →
// List(by label) → Stop → Remove.
package docker

import (
	"context"
	"testing"
	"time"
)

func TestIntegrationDockerClientLifecycle(t *testing.T) {
	ctx := context.Background()
	c := NewUnixClient("/var/run/docker.sock")
	const name = "crab-itest-lifecycle"
	const image = "alpine:3.20"

	// Clean any leftover from a previous aborted run.
	_ = c.Remove(ctx, name)

	if err := c.EnsureImage(ctx, image); err != nil {
		t.Fatalf("EnsureImage(pull): %v", err)
	}
	// Second call must hit the present-fast-path (no error, no needless pull —
	// verifies the un-escaped inspect path).
	if err := c.EnsureImage(ctx, image); err != nil {
		t.Fatalf("EnsureImage(present): %v", err)
	}

	spec := CreateSpec{
		Name:    name,
		Image:   image,
		Cmd:     []string{"sleep", "30"},
		Env:     []string{"FOO=bar"},
		Labels:  map[string]string{"crab-shell.managed": "true", "crab-shell.itest": "1"},
		Binds:   []string{"/tmp:/mnt/host:ro"},
		Network: "bridge",
		Init:    true,
	}
	if _, err := c.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = c.Remove(ctx, name) }()

	if err := c.Start(ctx, name); err != nil {
		t.Fatalf("Start: %v", err)
	}

	st, err := c.Inspect(ctx, name)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !st.Exists || !st.Running {
		t.Fatalf("after start: exists=%v running=%v, want true/true", st.Exists, st.Running)
	}

	sums, err := c.List(ctx, "crab-shell.itest=1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, s := range sums {
		for _, n := range s.Names {
			if n == "/"+name {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("List by label did not find %s (got %d containers)", name, len(sums))
	}

	if err := c.Stop(ctx, name, 3*time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	st, err = c.Inspect(ctx, name)
	if err != nil {
		t.Fatalf("Inspect after stop: %v", err)
	}
	if st.Running {
		t.Fatalf("after stop: running=true, want false")
	}

	if err := c.Remove(ctx, name); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	st, err = c.Inspect(ctx, name)
	if err != nil {
		t.Fatalf("Inspect after remove: %v", err)
	}
	if st.Exists {
		t.Fatalf("after remove: exists=true, want false")
	}
}
