// Command agent runs inside every engine image as the container's entrypoint.
// See internal/agent for the unit contract it implements.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/abhinandval/runpod-serverless-proxy/internal/agent"
)

func main() {
	log.SetFlags(log.Ltime | log.LUTC)
	cfg, engineName, engineCfg, err := agent.ConfigFromEnv(os.Getenv)
	if err != nil {
		log.Fatalf("agent: %v", err)
	}
	engine, err := agent.NewEngine(engineName, engineCfg)
	if err != nil {
		log.Fatalf("agent: %v", err)
	}
	dl := &agent.HFDownloader{Root: envOr("AGENT_MODELS_DIR", "/models"), Token: os.Getenv("HF_TOKEN"), Endpoint: os.Getenv("HF_ENDPOINT")}
	a := agent.New(cfg, engine, dl, nil)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Serve before anything else: RunPod must see /ping and the gateway must be
	// able to read the status while the model is still downloading.
	srv := &http.Server{Addr: cfg.Listen, Handler: a, ReadHeaderTimeout: 30 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("agent: listen %s: %v", cfg.Listen, err)
		}
	}()
	log.Printf("agent: serving on %s, engine %s on port %d", cfg.Listen, engineName, cfg.EnginePort)

	runErr := a.Run(ctx)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
	if runErr != nil {
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
