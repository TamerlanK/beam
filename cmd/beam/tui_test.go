package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/google/uuid"

	"github.com/TamerlanK/beam/internal/client"
	"github.com/TamerlanK/beam/internal/protocol"
	"github.com/TamerlanK/beam/internal/server"
)

func newTestTUI(peers ...protocol.Peer) *tui {
	e := &engine{o: &opts{}, r: client.NewReceiver(nil, ""), peers: peers,
		seen: map[uuid.UUID]bool{}, rows: map[uuid.UUID]*row{}, wake: func() {}}
	return newTUI(e)
}

var special = map[string]tea.KeyType{
	"up": tea.KeyUp, "down": tea.KeyDown, "enter": tea.KeyEnter, "esc": tea.KeyEsc,
	"backspace": tea.KeyBackspace, "tab": tea.KeyTab, "space": tea.KeySpace,
	"ctrl+u": tea.KeyCtrlU, "ctrl+w": tea.KeyCtrlW,
	"pgup": tea.KeyPgUp, "pgdown": tea.KeyPgDown, "home": tea.KeyHome, "end": tea.KeyEnd,
}

func keys(t *testing.T, m *tui, ks ...string) {
	t.Helper()
	for _, k := range ks {
		if typ, ok := special[k]; ok {
			m.Update(tea.KeyMsg{Type: typ})
		} else {
			m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
		}
	}
}

func TestCursorStaysInRange(t *testing.T) {
	m := newTestTUI(protocol.Peer{Name: "a"}, protocol.Peer{Name: "b"})
	keys(t, m, "down", "down", "down")
	if m.cursor != 1 {
		t.Fatalf("cursor ran past the last peer: got %d, want 1", m.cursor)
	}
	keys(t, m, "up", "up", "up")
	if m.cursor != 0 {
		t.Fatalf("cursor ran before the first peer: got %d, want 0", m.cursor)
	}

	m = newTestTUI()
	keys(t, m, "down", "up", "down")
	if m.cursor != 0 {
		t.Fatalf("cursor moved in an empty room: %d", m.cursor)
	}
	if _, ok := m.peer(); ok {
		t.Fatal("peer() returned a peer from an empty room")
	}
	keys(t, m, "s", "m")
	if m.prompt != nil {
		t.Fatal("a prompt opened with nobody to send to")
	}
	if last := m.s.log[len(m.s.log)-1]; !strings.Contains(last, "Nobody to send to") {
		t.Fatalf("expected a hint in the log, got %q", last)
	}
}

func TestPromptEditing(t *testing.T) {
	m := newTestTUI(protocol.Peer{ID: "p1", Name: "phone"})
	var got string
	m.ask("Note", false, func(_ *tui, s string) { got = s })

	keys(t, m, "a", "b", "x", "backspace", "c", "space", "d")
	if s := string(m.prompt.buf); s != "abc d" {
		t.Fatalf("prompt buffer: got %q, want %q", s, "abc d")
	}

	if m.cursor != 0 || m.prompt == nil {
		t.Fatal("keystrokes leaked out of the prompt into the command handler")
	}
	keys(t, m, "ctrl+w")
	if s := string(m.prompt.buf); s != "abc " {
		t.Fatalf("ctrl+w: got %q, want %q", s, "abc ")
	}
	keys(t, m, "ctrl+u")
	if len(m.prompt.buf) != 0 {
		t.Fatalf("ctrl+u left %q", string(m.prompt.buf))
	}

	keys(t, m, "h", "i", "enter")
	if got != "hi" {
		t.Fatalf("prompt result: got %q, want %q", got, "hi")
	}
	if m.prompt != nil {
		t.Fatal("prompt stayed open after enter")
	}

	got = ""
	m.ask("Note", false, func(_ *tui, s string) { got = s })
	keys(t, m, "h", "i", "esc")
	if m.prompt != nil || got != "" {
		t.Fatal("esc did not abandon the prompt")
	}
}

func TestTrimWord(t *testing.T) {
	for in, want := range map[string]string{
		"a/b/c": "a/b/", "a/b/c/": "a/b/", "one two": "one ", "one": "", "": "", "///": "",
	} {
		if got := string(trimWord([]rune(in))); got != want {
			t.Errorf("trimWord(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestComplete(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"alpha.txt", "alps.txt", "beta.bin", ".hidden", "x[1].txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "album"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := func(s string) string { return filepath.Join(dir, s) }
	sep := string(filepath.Separator)

	cases := []struct {
		in, want string
		names    []string
	}{
		{p("b"), p("beta.bin"), nil},
		{p("alp"), p("alp"), []string{"alpha.txt", "alps.txt"}},
		{p("a"), p("al"), nil},
		{p("alb"), p("album") + sep, nil},
		{p("zzz"), p("zzz"), nil},
		{p("x["), p("x[1].txt"), nil},
		{p(".h"), p(".hidden"), nil},
		{dir + sep, dir + sep, []string{"album" + sep, "alpha.txt", "alps.txt", "beta.bin", "x[1].txt"}},
	}
	for _, c := range cases {
		got, names := complete(c.in)
		if got != c.want || !reflect.DeepEqual(names, c.names) {
			t.Errorf("complete(%q) = %q, %v; want %q, %v", c.in, got, names, c.want, c.names)
		}
	}
}

func TestTabCompletesOnlyPathPrompts(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "only.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	m := newTestTUI(protocol.Peer{ID: "p1", Name: "phone"})
	keys(t, m, "s")
	if m.prompt == nil || !m.prompt.path {
		t.Fatal("s should open a path prompt")
	}
	m.prompt.buf = []rune(filepath.Join(dir, "on"))
	keys(t, m, "tab")
	if got := string(m.prompt.buf); got != filepath.Join(dir, "only.txt") {
		t.Fatalf("tab completed to %q", got)
	}

	keys(t, m, "esc", "m")
	m.prompt.buf = []rune(filepath.Join(dir, "on"))
	keys(t, m, "tab")
	if got := string(m.prompt.buf); got != filepath.Join(dir, "on") {
		t.Fatalf("tab changed a note to %q", got)
	}
}

func TestLogScrollback(t *testing.T) {
	m := newTestTUI()
	for i := range 50 {
		m.e.note("line %d", i)
	}
	m.sync()
	room := m.logRoom()
	if room >= 50 || room < 3 {
		t.Fatalf("test needs a log taller than the pane: room %d", room)
	}
	bottom := 50 - room

	keys(t, m, "pgup")
	if m.follow || m.logTop != max(0, bottom-room) {
		t.Fatalf("after PgUp: follow=%v top=%d, want top %d", m.follow, m.logTop, bottom-room)
	}
	keys(t, m, "home")
	if m.logTop != 0 || m.follow {
		t.Fatalf("after Home: top=%d follow=%v", m.logTop, m.follow)
	}
	keys(t, m, "pgdown")
	if m.logTop != room || m.follow {
		t.Fatalf("after PgDn: top=%d follow=%v, want %d", m.logTop, m.follow, room)
	}
	keys(t, m, "end")
	if !m.follow {
		t.Fatal("End should pin the log to new lines")
	}

	keys(t, m, "pgup")
	top := m.logTop
	m.e.note("newer")
	m.sync()
	if m.logTop != top || m.follow {
		t.Fatalf("view jumped on a new line: top %d→%d follow=%v", top, m.logTop, m.follow)
	}

	for range logCap {
		m.e.note("flood")
	}
	m.sync()
	if m.logTop != 0 || len(m.s.log) != logCap {
		t.Fatalf("after the cap: top=%d len=%d", m.logTop, len(m.s.log))
	}
}

func TestRowsLingerThenHide(t *testing.T) {
	e := newTestTUI().e
	id := uuid.New()
	now := time.Now()
	st := client.Status{ID: id, Label: "a.bin", Size: 100, Done: 40, State: client.Active, Peer: protocol.Peer{Name: "phone"}}

	e.upsert(st, true, now)
	if len(e.order) != 1 || e.rows[id].Peer.Name != "phone" {
		t.Fatalf("active transfer not shown: %+v", e.order)
	}
	st.Peer = protocol.Peer{}
	st.Done, st.State = 100, client.Done
	e.upsert(st, true, now)
	e.expire(now)
	if len(e.order) != 1 || e.rows[id].Peer.Name != "phone" || e.rows[id].State != client.Done {
		t.Fatalf("finished transfer should linger: order=%v row=%+v", e.order, e.rows[id])
	}
	e.expire(now.Add(linger))
	if len(e.order) != 0 || !e.rows[id].hidden {
		t.Fatal("finished transfer should hide after lingering")
	}
	e.upsert(st, true, now.Add(linger+time.Second))
	if len(e.order) != 0 {
		t.Fatal("a hidden transfer came back")
	}

	failed := client.Status{ID: uuid.New(), Label: "b.bin", State: client.Failed, Err: errors.New("declined")}
	e.upsert(failed, true, now)
	if len(e.order) != 0 {
		t.Fatal("a transfer that ended before it was shown should not appear")
	}

	e.queued = 0
	e.upsert(client.Status{ID: uuid.New(), State: client.Queued}, true, now)
	if e.queued != 1 || len(e.order) != 0 {
		t.Fatalf("queued files are counted, not shown: queued=%d order=%v", e.queued, e.order)
	}
}

func TestLiveRowsComeBeforeLingeringOnes(t *testing.T) {
	e := newTestTUI().e
	now := time.Now()
	done := client.Status{ID: uuid.New(), Label: "old.bin", Size: 1, Done: 1, State: client.Active}
	e.upsert(done, false, now)
	done.State = client.Done
	e.upsert(done, false, now)
	live := client.Status{ID: uuid.New(), Label: "new.bin", Size: 10, Done: 1, State: client.Active}
	e.upsert(live, true, now)

	s := e.snapshot(1)
	if len(s.rows) != 1 || s.rows[0].Label != "new.bin" || s.more != 1 {
		t.Fatalf("a lingering row hid the live one: rows=%+v more=%d", s.rows, s.more)
	}
	if s = e.snapshot(5); len(s.rows) != 2 || s.rows[0].Label != "new.bin" || s.rows[1].Label != "old.bin" || s.more != 0 {
		t.Fatalf("unexpected order or count: %+v more=%d", s.rows, s.more)
	}
}

func TestViewFitsTheWindow(t *testing.T) {
	m := newTestTUI(protocol.Peer{Name: "phone", Emoji: "📱", Device: "phone"})
	m.w, m.h = 60, 16
	for i := range 40 {
		m.e.note("a fairly long log line number %d that will need truncating at some point", i)
	}
	for i := range 12 {
		m.e.upsert(client.Status{ID: uuid.New(), Label: fmt.Sprintf("file-%d.bin", i), Size: 10, Done: 5, State: client.Active, SAS: "ABCD"}, i%2 == 0, time.Now())
	}
	m.e.queued = 3
	m.sync()
	if len(m.s.rows) != m.maxRows() || m.s.more != 12-m.maxRows() {
		t.Fatalf("snapshot rows=%d more=%d, want %d and %d", len(m.s.rows), m.s.more, m.maxRows(), 12-m.maxRows())
	}
	for _, m.prompt = range []*prompt{nil, {label: "Send path", buf: []rune("/some/path"), hint: "a.txt  b.txt"}} {
		out := m.View()
		lines := strings.Split(out, "\n")
		if len(lines) != m.h {
			t.Fatalf("view is %d lines, window is %d:\n%s", len(lines), m.h, out)
		}
		for i, l := range lines {
			if w := lipgloss.Width(l); w > m.w {
				t.Errorf("line %d is %d wide, window is %d: %q", i, w, m.w, l)
			}
		}
		if !strings.Contains(out, "7 more") || !strings.Contains(out, "3 queued") {
			t.Errorf("overflow line missing:\n%s", out)
		}
	}
}

func TestBar(t *testing.T) {
	if got := bar(4, 0.5); got != "██░░" {
		t.Errorf("bar(4, .5) = %q", got)
	}
	if got := bar(4, 2); got != "████" {
		t.Errorf("bar clamps above 1: %q", got)
	}
	if got := bar(4, -1); got != "░░░░" {
		t.Errorf("bar clamps below 0: %q", got)
	}
}

func TestGroupTakesOnlyTheFirstSendersOffers(t *testing.T) {
	a := protocol.Peer{ID: "a", Name: "laptop"}
	b := protocol.Peer{ID: "b", Name: "phone"}
	pend := []*client.Offer{{Name: "1", From: a}, {Name: "2", From: b}, {Name: "3", From: a}}

	got := group(pend)
	if len(got) != 2 || got[0].Name != "1" || got[1].Name != "3" {
		t.Fatalf("one answer must cover only the first sender's offers, got %v", got)
	}
}

func TestCleanStripsTerminalEscapes(t *testing.T) {
	if got := clean("ok\x1b[31mred\x1b[0m\r\n\x7f\u0085end"); got != "okredend" {
		t.Fatalf("clean = %q", got)
	}
}

func startTestServer(t *testing.T) string {
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

func dialTest(t *testing.T, addr, name string) (*client.Client, context.CancelFunc, chan error) {
	t.Helper()
	c, err := client.New(client.Config{Server: addr, Identity: client.Identity{ID: uuid.NewString(), Name: name, Emoji: "🦊"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	return c, cancel, done
}

func TestEngineDrainsWhileTheUIIsSlow(t *testing.T) {
	addr := startTestServer(t)
	src, dst := t.TempDir(), t.TempDir()
	var files []client.File
	for i := range 40 {
		name := fmt.Sprintf("part-%02d.bin", i)
		data := make([]byte, 512*1024)
		_, _ = rand.Read(data)
		if err := os.WriteFile(filepath.Join(src, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
		f, err := client.Stat(filepath.Join(src, name))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}

	rc, rcancel, rdone := dialTest(t, addr, "terminal")
	o := &opts{server: addr, wait: time.Minute}
	sn := &session{c: rc, o: o, cancel: rcancel, done: rdone}
	e := newEngine(sn, o, dst)
	go e.run()
	defer func() { _ = e.stop() }()
	for start := time.Now(); e.snapshot(1).self.ID == ""; {
		if time.Since(start) > 10*time.Second {
			t.Fatal("the terminal never connected")
		}
		time.Sleep(10 * time.Millisecond)
	}

	uiStop := make(chan struct{})
	defer close(uiStop)
	go func() {
		for {
			select {
			case <-uiStop:
				return
			case <-time.After(20 * time.Millisecond):
				_ = e.snapshot(10)
				time.Sleep(150 * time.Millisecond)
			}
		}
	}()

	sc, scancel, sdone := dialTest(t, addr, "sender")
	defer func() { scancel(); <-sdone }()
	if ev := <-sc.Events; ev.Type != client.Connected {
		t.Fatalf("sender: first event %q", ev.Type)
	}
	snd := client.NewSender(sc, rc.Self().ID, files)
	result := make(chan int, 1)
	go func() {
		snd.Start()
		timeout := time.After(60 * time.Second)
		for !snd.Finished() {
			select {
			case ev, ok := <-sc.Events:
				if !ok {
					result <- -1
					return
				}
				snd.Handle(ev)
			case <-time.After(time.Second):
				snd.Reap(time.Minute)
			case <-timeout:
				result <- -2
				return
			}
		}
		result <- snd.Failed()
	}()

	deadline := time.After(60 * time.Second)
	for {
		select {
		case failed := <-result:
			if failed != 0 {
				t.Fatalf("%d of %d files were not delivered (negative = sender stopped)", failed, len(files))
			}
			for _, f := range files {
				a, _ := os.ReadFile(f.Path)
				b, err := os.ReadFile(filepath.Join(dst, f.Name))
				if err != nil || string(a) != string(b) {
					t.Fatalf("%s: not delivered intact: %v", f.Name, err)
				}
			}
			return
		case <-deadline:
			t.Fatal("timeout")
		case <-time.After(20 * time.Millisecond):
			if pend := e.snapshot(1).pending; len(pend) > 0 {
				e.answer(group(pend), true)
			}
		}
	}
}
