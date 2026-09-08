package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/TamerlanK/beam/internal/protocol"
	"github.com/TamerlanK/beam/internal/server"
	"github.com/google/uuid"
)

func startServer(t *testing.T) string {
	t.Helper()
	s, err := server.New(server.Config{Addr: "127.0.0.1:0", Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Run(ctx)
	}()
	t.Cleanup(func() { cancel(); <-done })
	return s.Addr()
}

func dial(t *testing.T, addr, name string) *Client {
	t.Helper()
	c, err := New(Config{Server: addr, Identity: Identity{ID: uuid.NewString(), Name: name, Emoji: "🦊"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	if ev := <-c.Events; ev.Type != Connected {
		t.Fatalf("%s: first event %q, want connected", name, ev.Type)
	}
	return c
}

// dropConn kills the current socket; Run reconnects with the same identity.
func (c *Client) dropConn() {
	c.mu.Lock()
	if c.conn != nil {
		_ = c.conn.Close()
	}
	c.mu.Unlock()
}

func writeFile(t *testing.T, dir, name string, size int) []byte {
	t.Helper()
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return b
}

type got struct {
	name, path, sas string
	err             error
}

// runReceiver accepts everything and reports each finished transfer.
func runReceiver(rc *Client, dir string, results chan<- got, hook func(*Receiver)) {
	r := NewReceiver(rc, dir)
	sas := map[string]string{}
	r.OnStart = func(name string, _ protocol.Peer, s string) { sas[name] = s }
	r.OnDone = func(name string, _ protocol.Peer, path string, err error) {
		results <- got{name, path, sas[name], err}
	}
	if hook != nil {
		hook(r)
	}
	go func() {
		for ev := range rc.Events {
			r.Handle(ev)
			for _, o := range r.Pending() {
				_ = r.Answer(o, true)
			}
		}
	}()
}

// runSender drives a Sender to the end and returns the verification codes
// it showed, per file name, and how many times a transfer started.
func runSender(t *testing.T, sc *Client, to string, files []File) (map[string][]string, int) {
	t.Helper()
	s := NewSender(sc, to, files)
	sas := map[string][]string{}
	starts := 0
	s.OnStart = func(f File, code string) { sas[f.Name] = append(sas[f.Name], code); starts++ }
	s.OnDone = func(f File, err error) {
		if err != nil {
			t.Errorf("%s: %v", f.Name, err)
		}
	}
	s.Start()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	timeout := time.After(30 * time.Second)
	for !s.Finished() {
		select {
		case ev, ok := <-sc.Events:
			if !ok {
				t.Fatal("sender client stopped")
			}
			s.Handle(ev)
		case <-tick.C:
			s.Reap(20 * time.Second)
		case <-timeout:
			t.Fatal("timeout waiting for the send to finish")
		}
	}
	return sas, starts
}

func TestSendReceive(t *testing.T) {
	addr := startServer(t)
	src, dst := t.TempDir(), t.TempDir()
	want := map[string][]byte{
		"a.txt": writeFile(t, src, "a.txt", 1),
		"b.bin": writeFile(t, src, "b.bin", protocol.ChunkSize),
		"c.bin": writeFile(t, src, "c.bin", 3*protocol.ChunkSize+7),
	}
	rc := dial(t, addr, "receiver")
	sc := dial(t, addr, "sender")
	results := make(chan got, 8)
	runReceiver(rc, dst, results, nil)

	names := []string{"a.txt", "b.bin", "c.bin", "a.txt"} // a.txt twice: the second must not overwrite
	var files []File
	for _, n := range names {
		f, err := Stat(filepath.Join(src, n))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	sent, _ := runSender(t, sc, rc.Self().ID, files)

	seen := map[string][]string{}
	for range files {
		var g got
		select {
		case g = <-results:
		case <-time.After(30 * time.Second):
			t.Fatal("timeout waiting for the receiver")
		}
		if g.err != nil {
			t.Fatalf("%s: %v", g.name, g.err)
		}
		data, err := os.ReadFile(g.path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, want[g.name]) {
			t.Errorf("%s: content differs (%d bytes, want %d)", g.name, len(data), len(want[g.name]))
		}
		if g.sas == "" {
			t.Errorf("%s: not encrypted", g.name)
		}
		seen[g.name] = append(seen[g.name], g.sas)
	}
	for name, codes := range seen {
		sort.Strings(codes)
		sort.Strings(sent[name])
		if len(codes) != len(sent[name]) || !equal(codes, sent[name]) {
			t.Errorf("%s: verification codes differ: receiver %v, sender %v", name, codes, sent[name])
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "a (1).txt")); err != nil {
		t.Errorf("second a.txt should be saved as a (1).txt: %v", err)
	}
	if parts, _ := filepath.Glob(filepath.Join(dst, ".beam-*.part")); len(parts) > 0 {
		t.Errorf("part files left behind: %v", parts)
	}
}

func TestResume(t *testing.T) {
	addr := startServer(t)
	src, dst := t.TempDir(), t.TempDir()
	want := writeFile(t, src, "big.bin", 24<<20)
	rc := dial(t, addr, "receiver")
	sc := dial(t, addr, "sender")
	results := make(chan got, 1)
	dropped := make(chan struct{})
	runReceiver(rc, dst, results, func(r *Receiver) {
		var once sync.Once
		r.OnProgress = func() {
			if n, _ := r.Progress(); n > 2<<20 {
				once.Do(func() {
					rc.dropConn()
					close(dropped)
				})
			}
		}
	})
	f, err := Stat(filepath.Join(src, "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	_, starts := runSender(t, sc, rc.Self().ID, []File{f})
	<-dropped
	var g got
	select {
	case g = <-results:
	case <-time.After(30 * time.Second):
		t.Fatal("timeout waiting for the receiver")
	}
	if g.err != nil {
		t.Fatal(g.err)
	}
	data, err := os.ReadFile(g.path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, want) {
		t.Errorf("content differs after resume (%d bytes, want %d)", len(data), len(want))
	}
	if starts < 2 {
		t.Errorf("transfer started %d times, want a re-offer after the drop", starts)
	}
}

func TestSafeName(t *testing.T) {
	cases := map[string]string{
		"holiday.mp4":          "holiday.mp4",
		"../../etc/passwd":     "passwd",
		`C:\Users\x\notes.txt`: "notes.txt",
		"a\x00b\x1f.txt":       "a_b_.txt",
		"  ..  ":               "file",
		"":                     "file",
	}
	for in, want := range cases {
		if got := safeName(in); got != want {
			t.Errorf("safeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func equal(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
