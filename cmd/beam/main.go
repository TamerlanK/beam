package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/TamerlanK/beam/internal/server"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	debug := flag.Bool("debug", false, "enable pprof on /debug/pprof/")
	logFormat := flag.String("log-format", "text", "log format: text or json")
	contact := flag.String("contact", "", "operator contact shown on /privacy (email or URL)")
	trustProxy := flag.Bool("trust-proxy", false, "trust X-Forwarded-For for room grouping (only behind a trusted proxy)")
	flag.Parse()

	var log *slog.Logger
	if *logFormat == "json" {
		log = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	} else {
		log = slog.New(&textHandler{w: os.Stderr, mu: new(sync.Mutex)})
	}
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, err := server.New(server.Config{
		Addr:       *addr,
		Debug:      *debug,
		TrustProxy: *trustProxy,
		Contact:    *contact,
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

type textHandler struct {
	w     io.Writer
	mu    *sync.Mutex
	attrs string
}

func (h *textHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *textHandler) WithGroup(string) slog.Handler { return h }

func (h *textHandler) WithAttrs(as []slog.Attr) slog.Handler {
	n := *h
	for _, a := range as {
		n.attrs += fmtAttr(a)
	}
	return &n
}

func (h *textHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %-5s %-36s", r.Time.Format("15:04:05"), r.Level, r.Message)
	b.WriteString(h.attrs)
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString(fmtAttr(a))
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, strings.TrimRight(b.String(), " ")+"\n")
	return err
}

func fmtAttr(a slog.Attr) string {
	if a.Equal(slog.Attr{}) {
		return ""
	}
	v := a.Value.String()
	if v == "" || strings.ContainsAny(v, " \"=") {
		v = strconv.Quote(v)
	}
	return "  " + a.Key + "=" + v
}
