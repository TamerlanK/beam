package main

import (
	"context"
	"errors"
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
	"time"

	"github.com/TamerlanK/beam/internal/hub"
	"github.com/TamerlanK/beam/internal/server"
)

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		os.Exit(serve(args))
	case "ls", "send", "recv":
		os.Exit(runCLI(cmd, args))
	case "tui":
		os.Exit(runTUI(args))
	case "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "beam: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
}

func serve(args []string) int {
	fs := flag.NewFlagSet("beam serve", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, usage, "\nServer flags:\n")
		fs.PrintDefaults()
	}
	addr := fs.String("addr", ":8080", "listen address")
	debug := fs.Bool("debug", false, "enable pprof on /debug/pprof/")
	logFormat := fs.String("log-format", "text", "log format: text or json")
	contact := fs.String("contact", "", "operator contact shown on /privacy (email or URL)")
	maxConns := fs.Int("max-conns-per-ip", 32, "max concurrent websocket connections per IP (0 = unlimited)")
	relayBPS := fs.Int64("relay-bps", 0, "global relay budget in bytes per second (0 = unlimited)")
	trustProxy := fs.Bool("trust-proxy", false, "trust X-Forwarded-For for room grouping (only behind a trusted proxy)")
	dropMax := fs.Int64("drop-max", 200<<20, "max bytes per drop held for later pickup (0 = disable drops)")
	dropBudget := fs.Int64("drop-budget", 1<<30, "max bytes of drops held in memory at once (0 = unlimited)")
	dropTTL := fs.Duration("drop-ttl", 10*time.Minute, "how long a drop waits to be picked up")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

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
		Limits: hub.Limits{
			MaxConnsPerIP: *maxConns, RelayBytesPerSec: *relayBPS,
			MaxDropBytes: *dropMax, DropBudget: *dropBudget, DropTTL: *dropTTL,
		},
		Log: log,
	})
	if err != nil {
		log.Error("startup failed", "err", err)
		return 1
	}
	if err := srv.Run(ctx); err != nil {
		log.Error("server error", "err", err)
		return 1
	}
	return 0
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
