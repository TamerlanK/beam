package server

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/TamerlanK/beam/internal/hub"
	"github.com/TamerlanK/beam/web"
)

type Config struct {
	Addr string

	Debug bool

	TrustProxy bool

	Log *slog.Logger
}

type Server struct {
	cfg  Config
	log  *slog.Logger
	hub  *hub.Hub
	http *http.Server
	ln   net.Listener
}

func New(cfg Config) (*Server, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, err
	}

	s := &Server{cfg: cfg, log: cfg.Log, ln: ln, hub: hub.New(cfg.Log)}

	staticFS, err := fs.Sub(web.Static, "static")
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServerFS(staticFS))
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		hub.ServeWS(s.hub, w, r, cfg.TrustProxy)
	})
	mux.HandleFunc("/share", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	if cfg.Debug {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}

	s.http = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s, nil
}

func (s *Server) Addr() string { return s.ln.Addr().String() }

func (s *Server) Run(ctx context.Context) error {
	hubCtx, stopHub := context.WithCancel(context.Background())
	hubDone := make(chan struct{})
	go func() {
		defer close(hubDone)
		s.hub.Run(hubCtx)
	}()

	errc := make(chan error, 1)
	go func() { errc <- s.http.Serve(s.ln) }()
	s.log.Info("server listening", "addr", s.Addr())

	select {
	case err := <-errc:
		stopHub()
		<-hubDone
		return err
	case <-ctx.Done():
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := s.http.Shutdown(shutCtx)
	stopHub()
	<-hubDone
	if serveErr := <-errc; !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	s.log.Info("server stopped")
	return err
}
