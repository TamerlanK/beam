package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/TamerlanK/beam/internal/server"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	debug := flag.Bool("debug", false, "enable pprof on /debug/pprof/")
	trustProxy := flag.Bool("trust-proxy", false, "trust X-Forwarded-For for room grouping (only behind a trusted proxy)")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, err := server.New(server.Config{
		Addr:       *addr,
		Debug:      *debug,
		TrustProxy: *trustProxy,
		Log:        log,
	})
	if err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}
	if err := srv.Run(ctx); err != nil {
		log.Error("server error", "err", err)
		os.Exit(1)
	}
}
