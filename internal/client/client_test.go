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
	"sync/atomic"
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

	names := []string{"a.txt", "b.bin", "c.bin", "a.txt"}
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

func TestSendFolder(t *testing.T) {
	addr := startServer(t)
	src, dst := t.TempDir(), t.TempDir()
	root := filepath.Join(src, "photos")
	deep := filepath.Join(root, "2024", "sub")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	want := map[string][]byte{
		"photos/a.jpg":          writeFile(t, root, "a.jpg", 5),
		"photos/2024/b.bin":     writeFile(t, filepath.Join(root, "2024"), "b.bin", protocol.ChunkSize+3),
		"photos/2024/sub/c.txt": writeFile(t, deep, "c.txt", 77),
	}
	if err := os.WriteFile(filepath.Join(root, "empty.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	files, skipped, err := Expand(root)
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 1 || len(files) != len(want) {
		t.Fatalf("Expand: %d files, %d skipped; want %d and 1", len(files), skipped, len(want))
	}
	for _, f := range files {
		if _, ok := want[f.Label()]; !ok {
			t.Fatalf("Expand produced unexpected label %q", f.Label())
		}
	}

	rc := dial(t, addr, "receiver")
	sc := dial(t, addr, "sender")
	results := make(chan got, 8)
	runReceiver(rc, dst, results, nil)
	runSender(t, sc, rc.Self().ID, files)

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
		if wantPath := filepath.Join(dst, filepath.FromSlash(g.name)); g.path != wantPath {
			t.Errorf("%s saved at %q, want %q", g.name, g.path, wantPath)
		}
		data, err := os.ReadFile(g.path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, want[g.name]) {
			t.Errorf("%s: content differs", g.name)
		}
	}
	if parts, _ := filepath.Glob(filepath.Join(dst, ".beam-*.part")); len(parts) > 0 {
		t.Errorf("part files left behind: %v", parts)
	}
}

func TestBatchDeclinedOnce(t *testing.T) {
	addr := startServer(t)
	src, dst := t.TempDir(), t.TempDir()
	var files []File
	for _, n := range []string{"a.bin", "b.bin", "c.bin"} {
		writeFile(t, src, n, 10)
		f, err := Stat(filepath.Join(src, n))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	rc := dial(t, addr, "receiver")
	sc := dial(t, addr, "sender")

	r := NewReceiver(rc, dst)
	var asked atomic.Int32
	go func() {
		for ev := range rc.Events {
			r.Handle(ev)
			for _, o := range r.Pending() {
				asked.Add(1)
				_ = r.Answer(o, false)
			}
		}
	}()

	s := NewSender(sc, rc.Self().ID, files)
	declined := 0
	s.OnDone = func(_ File, err error) {
		if err != nil && err.Error() == "declined" {
			declined++
		}
	}
	s.Start()
	deadline := time.After(15 * time.Second)
	for !s.Finished() {
		select {
		case ev, ok := <-sc.Events:
			if !ok {
				t.Fatal("sender client stopped")
			}
			s.Handle(ev)
		case <-deadline:
			t.Fatal("sender never finished after a decline")
		}
	}
	if declined != len(files) {
		t.Errorf("%d transfers declined, want %d", declined, len(files))
	}
	if n := asked.Load(); n != 1 {
		t.Errorf("receiver was asked %d times, want 1: a declined batch must be declined silently", n)
	}
}

func TestSafePath(t *testing.T) {
	cases := map[string]string{
		"":                "",
		"photos":          "photos",
		"photos/2024":     "photos/2024",
		`photos\2024`:     "photos/2024",
		"../x":            "",
		"a/../b":          "",
		"/abs":            "",
		"a/ .. /b":        "a/file/b",
		"a/b\x00c/d":      "a/b_c/d",
		"a/" + "b/":       "",
		"a/" + "./" + "b": "",
	}
	for in, want := range cases {
		if got := safePath(in); got != want {
			t.Errorf("safePath(%q) = %q, want %q", in, got, want)
		}
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

func TestXfersReportEveryFile(t *testing.T) {
	addr := startServer(t)
	src, dst := t.TempDir(), t.TempDir()
	writeFile(t, src, "a.bin", 2*protocol.ChunkSize+1)
	writeFile(t, src, "b.bin", 5)
	rc := dial(t, addr, "receiver")
	sc := dial(t, addr, "sender")
	results := make(chan got, 4)
	var r *Receiver
	runReceiver(rc, dst, results, func(x *Receiver) { r = x })

	var files []File
	for _, n := range []string{"a.bin", "b.bin"} {
		f, err := Stat(filepath.Join(src, n))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	s := NewSender(sc, rc.Self().ID, files)
	if got := s.Xfers(); len(got) != 2 || got[0].State != Queued || got[0].Peer.Name != "receiver" || got[0].Label != "a.bin" {
		t.Fatalf("before start: %+v", got)
	}
	s.Start()
	timeout := time.After(30 * time.Second)
	for !s.Finished() {
		select {
		case ev := <-sc.Events:
			s.Handle(ev)
		case <-timeout:
			t.Fatal("timeout")
		}
	}
	for range files {
		select {
		case g := <-results:
			if g.err != nil {
				t.Fatal(g.err)
			}
		case <-timeout:
			t.Fatal("timeout")
		}
	}

	sent, recv := s.Xfers(), r.Xfers()
	if len(sent) != 2 || len(recv) != 2 {
		t.Fatalf("want 2 statuses each side, got %d and %d", len(sent), len(recv))
	}
	for i := range sent {
		a, b := sent[i], recv[i]
		if a.ID != b.ID || a.Label != b.Label || a.Size != b.Size {
			t.Errorf("status %d: sides disagree: %+v vs %+v", i, a, b)
		}
		if a.State != Done || b.State != Done || a.Done != a.Size || b.Done != b.Size {
			t.Errorf("status %d: not finished: %+v vs %+v", i, a, b)
		}
		if a.SAS == "" || a.SAS != b.SAS {
			t.Errorf("status %d: verification codes differ: %q vs %q", i, a.SAS, b.SAS)
		}
		if !a.State.Terminal() || a.State.String() != "done" {
			t.Errorf("status %d: State helpers wrong for %v", i, a.State)
		}
	}
}
