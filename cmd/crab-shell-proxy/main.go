// Command crab-shell-proxy is a Go orchestrator that sits behind the mycelium
// gateway and, per request, ensures a per-(agent,user) picoclaw container is
// running, then speaks the Pico Protocol to it while presenting an
// OpenAI-compatible HTTP surface. See .specs/features/crab-shell-proxy.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/httpapi"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/memgraph"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/pico"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/registry"
)

func main() {
	logger := log.New(os.Stdout, "crab-shell-proxy ", log.LstdFlags|log.Lmsgprefix)

	configPath := os.Getenv("CRAB_CONFIG")
	if configPath == "" {
		configPath = "/etc/crab-shell-proxy/config.yaml"
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		logger.Fatalf("config: %v", err)
	}

	socket := os.Getenv("DOCKER_SOCKET")
	if socket == "" {
		socket = "/var/run/docker.sock"
	}
	dkr := docker.NewUnixClient(socket)

	regPath := filepath.Join(cfg.ContainerDataRoot, "model-registry.db")
	reg, err := registry.Open(regPath, nil)
	if err != nil {
		logger.Fatalf("open model registry: %v", err)
	}
	defer func() { _ = reg.Close() }()

	mgr := docker.NewManager(cfg, dkr, nil, reg, logger.Printf)

	srv := &httpapi.Server{
		Cfg:      cfg,
		Resolver: identity.NewSDKResolver(),
		Mgr:      mgr,
		Pico:     &pico.Client{IdleTimeout: cfg.TurnIdleTimeout.Std()},
		Logf:     logger.Printf,
		Reg:      reg,
		// The knowledge-graph memory. Rooted at the CONTAINER data root because this
		// process reads and writes the files itself; the host root is only ever a
		// bind-mount source handed to the Docker daemon.
		//
		// Always constructed, even with no signing secret: the read-only
		// /v1/memory-graph routes work for a graph that already exists, while the MCP
		// endpoint the agent writes through is mounted only when the secret is set
		// (see Handler). So turning the secret off stops new memories being written
		// without hiding what a member already has.
		MemoryGraph: memgraph.NewStore(cfg.ContainerDataRoot, time.Now),
	}
	if cfg.ResolvedMCPTokenSecret == "" {
		logger.Printf("memory graph: CRAB_MCP_TOKEN_SECRET is unset — " +
			"the native MCP endpoint is NOT mounted and no agent will be given memory")
	}

	// The model-inventory migration runs SYNCHRONOUSLY, before the server listens.
	// It is the only thing here that a request can race: a chat arriving while the
	// inventory is still empty resolves no model and fails, and on first boot after
	// upgrade that is every chat. It is bounded work (reads the data root once), so
	// it delays readiness by a little; the container work below is what could take
	// minutes and stays in the background.
	//
	// A failure is fatal on purpose: serving with a half-seeded inventory means
	// refusing or silently re-resolving workspaces, which is worse than not coming
	// up. The pass withholds its schema marker on any capture failure, so a restart
	// retries the whole thing.
	if err := mgr.MigrateModels(); err != nil {
		logger.Fatalf("migrate model registry: %v", err)
	}

	// Reconcile (drift check, adopt running containers, re-arm timers, start
	// continuous ones) runs in the background so /healthz is responsive
	// immediately — a long continuous-ensure over many user dirs must not delay
	// readiness (the compose healthcheck and mycelium's dependency gate both poll
	// /healthz).
	go func() {
		reconcileCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := mgr.Reconcile(reconcileCtx); err != nil {
			logger.Printf("reconcile (non-fatal): %v", err)
		}
	}()

	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: streaming turns and cold starts can outlast any
		// fixed deadline; the per-turn timeout bounds them instead.
	}

	go func() {
		logger.Printf("listening on %s (agents: %d)", cfg.Listen, len(cfg.Agents))
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("serve: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	logger.Printf("shutting down")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	_ = httpServer.Shutdown(shutdownCtx)
}
