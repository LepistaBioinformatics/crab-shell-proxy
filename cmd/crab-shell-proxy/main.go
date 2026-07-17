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
	"syscall"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/httpapi"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/pico"
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
	mgr := docker.NewManager(cfg, dkr, nil, logger.Printf)

	srv := &httpapi.Server{
		Cfg:      cfg,
		Resolver: identity.NewSDKResolver(),
		Mgr:      mgr,
		Pico:     &pico.Client{TurnTimeout: cfg.TurnTimeout.Std()},
		Logf:     logger.Printf,
	}

	// Reconcile (adopt running containers, re-arm timers, start continuous ones)
	// runs in the background so /healthz is responsive immediately — a long
	// continuous-ensure over many user dirs must not delay readiness (the
	// compose healthcheck and mycelium's dependency gate both poll /healthz).
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
