// Command gateway serves the client API: OpenAI- and Anthropic-style requests
// in, serverless units out. See docs/design.md.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/abhinandval/runpod-serverless-proxy/internal/config"
	"github.com/abhinandval/runpod-serverless-proxy/internal/gateway"
)

func main() {
	log.SetFlags(log.Ltime | log.LUTC)
	path := flag.String("config", "config.yaml", "path to the configuration file")
	flag.Parse()

	cfg, err := config.Load(*path)
	if err != nil {
		log.Fatalf("gateway: %v", err)
	}
	if cfg.Fleet.InferenceKey == "" {
		log.Fatal("gateway: fleet.inference_key is empty (is the environment variable it refers to set?)")
	}
	g, err := gateway.New(cfg, nil)
	if err != nil {
		log.Fatalf("gateway: %v", err)
	}

	// No write timeout: a response can be a stream that waits out a cold start.
	srv := &http.Server{Addr: cfg.Gateway.Listen, Handler: g, ReadHeaderTimeout: 30 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	log.Printf("gateway: serving %d models from %d units on %s", len(cfg.Models), len(cfg.Units), cfg.Gateway.Listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("gateway: %v", err)
	}
}
