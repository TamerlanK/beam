package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/google/uuid"

	"github.com/TamerlanK/beam/internal/client"
	"github.com/TamerlanK/beam/internal/protocol"
)

const (
	linger    = 3 * time.Second
	logCap    = 1000
	meterRate = 100 * time.Millisecond
	uiRate    = 50 * time.Millisecond
)

func runTUI(args []string) int {
	var o opts
	fs := flag.NewFlagSet("beam tui", flag.ContinueOnError)
	o.flags(fs, true)
	dir := fs.String("dir", ".", "directory to save received files into")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "beam: %v\n", err)
		return 1
	}
	s, err := o.connect()
	if err != nil {
		fmt.Fprintf(os.Stderr, "beam: %v\n", err)
		return 1
	}

	e := newEngine(s, &o, *dir)
	p := tea.NewProgram(newTUI(e), tea.WithAltScreen())
	wake := make(chan struct{}, 1)
	e.wake = func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	go e.run()
	go func() {

		for range wake {
			p.Send(refreshMsg{})
			time.Sleep(uiRate)
		}
	}()
	_, err = p.Run()
	fatal := e.stop()
	close(wake)
	if err != nil {
		fmt.Fprintf(os.Stderr, "beam: %v\n", err)
		return 1
	}
	if fatal != nil {
		fmt.Fprintf(os.Stderr, "beam: %v\n", fatal)
		return 1
	}
	return 0
}

type row struct {
	client.Status
	out    bool
	m      *meter
	rate   float64
	doneAt time.Time
	hidden bool
}

type engine struct {
	mu  sync.Mutex
	sn  *session
	o   *opts
	r   *client.Receiver
	snd []*client.Sender

	self    protocol.Peer
	peers   []protocol.Peer
	status  string
	code    string
	codeTTL string

	log     []string
	dropped int
	seen    map[uuid.UUID]bool

	rows      map[uuid.UUID]*row
	order     []uuid.UUID
	queued    int
	lastMeter time.Time

	done  bool
	fatal error

	wake     func()
	finished chan struct{}
}

func newEngine(sn *session, o *opts, dir string) *engine {
	e := &engine{sn: sn, o: o, status: "connecting…", seen: map[uuid.UUID]bool{},
		rows: map[uuid.UUID]*row{}, wake: func() {}, finished: make(chan struct{})}
	e.r = client.NewReceiver(sn.c, dir)
	e.r.OnStart = func(name string, from protocol.Peer, sas string) {
		e.say("Receiving %s from %s · %s", name, from.Name, verify(sas))
	}
	e.r.OnPause = func(name string, from protocol.Peer) {
		e.say("%s: %s went away, waiting for them to come back", name, from.Name)
	}
	e.r.OnDone = func(name string, from protocol.Peer, path string, err error) {
		if err != nil {
			e.say("%s from %s: %v", name, from.Name, err)
			return
		}
		e.say("Saved %s", path)
	}
	e.r.OnNote = func(from protocol.Peer, text string) {
		lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
		e.say("Note from %s: %s", from.Name, lines[0])
		for _, l := range lines[1:] {
			e.say("    %s", l)
		}
	}
	e.r.OnExpired = func(off *client.Offer, reason string) {
		e.say("%s from %s: %s", off.Label(), off.From.Name, reason)
	}
	e.r.OnProgress = func() { e.refresh(false) }
	e.say("Saving into %s", dir)
	return e
}

func (e *engine) run() {
	defer close(e.finished)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case ev, ok := <-e.sn.c.Events:
			e.mu.Lock()
			if !ok {
				e.done, e.fatal = true, e.sn.close()
				e.mu.Unlock()
				e.wake()
				return
			}
			e.event(ev)
			e.mu.Unlock()
		case <-tick.C:
			e.mu.Lock()
			e.r.Reap(e.o.wait)
			for _, s := range e.snd {
				s.Reap(e.o.wait)
			}

			e.refresh(true)
			e.snd = prune(e.snd)
			e.mu.Unlock()
		}
		e.wake()
	}
}

func (e *engine) stop() error {
	e.mu.Lock()
	e.cancelAll()
	e.mu.Unlock()
	err := e.sn.close()
	<-e.finished
	return err
}

func (e *engine) cancelAll() {
	for _, s := range e.snd {
		s.Cancel()
	}
	e.r.Cancel()
	e.refresh(true)
}

func (e *engine) say(format string, a ...any) {
	e.log = append(e.log, time.Now().Format("15:04:05")+" "+clean(fmt.Sprintf(format, a...)))
	if n := len(e.log) - logCap; n > 0 {
		e.log = e.log[n:]
		e.dropped += n
	}
}

func (e *engine) note(format string, a ...any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.say(format, a...)
}

func (e *engine) event(ev client.Event) {
	switch ev.Type {
	case client.Connected:
		e.status = "connected"
		e.self = e.sn.c.Self()
		if e.sn.down {
			e.sn.down = false
			e.say("Reconnected")
		}
		e.sn.ready = true
		if e.o.share && e.code == "" {
			_ = e.sn.c.Send(protocol.TypeRoomCreate, nil)
		}
	case client.Disconnected:
		e.status, e.sn.ready, e.sn.down = "reconnecting…", false, true
		e.say("Connection lost, reconnecting…")
	case protocol.TypeRoomCreated:
		var m protocol.RoomCreated
		if json.Unmarshal(ev.Data, &m) == nil {
			e.code, e.codeTTL = m.Code, codeWindow(m.ExpiresIn)
			e.say("Room code %s — enter it on another device%s", m.Code, idleFor(m.ExpiresIn))
		}
	case protocol.TypeError:
		if err := serverError(ev); err != nil {
			e.say("%v", err)
		}
	}
	e.peers = e.sn.c.Peers()
	e.r.Handle(ev)
	for _, s := range e.snd {
		s.Handle(ev)
	}
	e.announce()
	e.refresh(false)
}

func (e *engine) announce() {
	for _, off := range e.r.Pending() {
		key := off.ID
		if off.Batch != nil {
			if id, err := uuid.Parse(off.Batch.ID); err == nil {
				key = id
			}
		}
		if e.seen[key] {
			continue
		}
		e.seen[key] = true
		if off.Batch != nil {
			e.say("%s offers %d files (%s) — y to accept, n to decline", off.From.Name, off.Batch.Files, client.HumanBytes(off.Batch.Bytes))
		} else {
			e.say("%s offers %s (%s) — y to accept, n to decline", off.From.Name, off.Label(), client.HumanBytes(off.Size))
		}
	}
}

func prune(all []*client.Sender) []*client.Sender {
	out := all[:0]
	for _, s := range all {
		if !s.Finished() {
			out = append(out, s)
		}
	}
	return out
}

func (e *engine) refresh(force bool) {
	now := time.Now()
	if !force && now.Sub(e.lastMeter) < meterRate {
		return
	}
	e.lastMeter = now
	e.queued = 0
	for _, s := range e.snd {
		for _, st := range s.Xfers() {
			e.upsert(st, true, now)
		}
	}
	for _, st := range e.r.Xfers() {
		e.upsert(st, false, now)
	}
	e.expire(now)
}

func (e *engine) upsert(st client.Status, out bool, now time.Time) {
	if st.State == client.Queued {
		e.queued++
		return
	}
	r := e.rows[st.ID]
	if r == nil {
		if st.State.Terminal() {
			return
		}
		r = &row{out: out, m: &meter{}}
		e.rows[st.ID] = r
		e.order = append(e.order, st.ID)
	}
	if st.Peer.Name == "" {
		st.Peer = r.Peer
	}
	r.Status = st
	if st.State == client.Active {
		r.rate = r.m.rate(st.Done)
	} else {
		r.rate = 0
	}
	if st.State.Terminal() && r.doneAt.IsZero() {
		r.doneAt = now
	}
}

func (e *engine) expire(now time.Time) {
	keep := make([]uuid.UUID, 0, len(e.order))
	for _, id := range e.order {
		r := e.rows[id]
		if !r.doneAt.IsZero() && now.Sub(r.doneAt) >= linger {
			r.hidden = true
			continue
		}
		keep = append(keep, id)
	}
	e.order = keep
}

func group(pend []*client.Offer) []*client.Offer {
	var out []*client.Offer
	for _, off := range pend {
		if off.From.ID == pend[0].From.ID {
			out = append(out, off)
		}
	}
	return out
}

func (e *engine) answer(offers []*client.Offer, accept bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	answered := 0
	for _, off := range offers {
		if err := e.r.Answer(off, accept); err != nil {
			if !errors.Is(err, client.ErrExpired) {
				e.say("%s: %v", off.Label(), err)
			}
			continue
		}
		answered++
	}
	if !accept && answered > 0 {
		e.say("Declined %d file(s) from %s", answered, offers[0].From.Name)
	}
	e.refresh(true)
}

func (e *engine) send(peer protocol.Peer, files []client.File) {
	e.mu.Lock()
	defer e.mu.Unlock()
	snd := client.NewSender(e.sn.c, peer.ID, files)
	snd.OnStart = func(f client.File, sas string) {
		e.say("Sending %s (%s) to %s · %s", f.Label(), client.HumanBytes(f.Size), peer.Name, verify(sas))
	}
	snd.OnPause = func(f client.File) { e.say("%s: paused, waiting for %s to come back", f.Label(), peer.Name) }
	snd.OnDone = func(f client.File, err error) {
		if err != nil {
			e.say("%s: %v", f.Label(), err)
			return
		}
		e.say("Sent %s", f.Label())
	}
	snd.OnProgress = func() { e.refresh(false) }
	e.snd = append(e.snd, snd)
	snd.Start()
	e.refresh(true)
}

func (e *engine) sendNote(peer protocol.Peer, text string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.sn.c.Send(protocol.TypeSnippet, protocol.Snippet{To: peer.ID, Text: text}); err != nil {
		e.say("%v", err)
		return
	}
	e.say("Note to %s: %s", peer.Name, text)
}

func (e *engine) join(code string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.sn.c.Send(protocol.TypeRoomJoin, protocol.RoomJoin{Code: code}); err != nil {
		e.say("%v", err)
		return
	}
	e.say("Joining room %s…", code)
}

func (e *engine) createCode() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.sn.c.Send(protocol.TypeRoomCreate, nil); err != nil {
		e.say("%v", err)
	}
}

func (e *engine) cancel() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cancelAll()
	e.say("Canceled everything in flight")
}

type snapshot struct {
	self    protocol.Peer
	peers   []protocol.Peer
	status  string
	code    string
	codeTTL string
	log     []string
	dropped int
	rows    []row
	more    int
	queued  int
	pending []*client.Offer
	done    bool
}

func (e *engine) snapshot(limit int) snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := snapshot{self: e.self, peers: e.peers, status: e.status, code: e.code, codeTTL: e.codeTTL,
		log: e.log, dropped: e.dropped, queued: e.queued, pending: e.r.Pending(), done: e.done}
	s.rows = make([]row, 0, min(len(e.order), limit))
	for _, live := range []bool{true, false} {
		for _, id := range e.order {
			if len(s.rows) == limit {
				break
			}
			if r := e.rows[id]; r.State.Terminal() != live {
				s.rows = append(s.rows, *r)
			}
		}
	}
	s.more = len(e.order) - len(s.rows)
	return s
}

func verify(sas string) string {
	if sas == "" {
		return "not encrypted"
	}
	return "verify " + sas
}

func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, ansi.Strip(s))
}

type refreshMsg struct{}

type prompt struct {
	label string
	buf   []rune
	path  bool
	hint  string
	done  func(*tui, string)
}

type tui struct {
	e *engine
	s snapshot

	cursor int
	prompt *prompt

	logTop      int
	follow      bool
	seenDropped int

	w, h int
}

func newTUI(e *engine) *tui {
	t := &tui{e: e, w: 80, h: 24, follow: true}
	t.sync()
	return t
}

func (t *tui) sync() {
	t.s = t.e.snapshot(t.maxRows())
	if n := t.s.dropped - t.seenDropped; n > 0 {
		t.logTop = max(0, t.logTop-n)
		t.seenDropped = t.s.dropped
	}
	if t.cursor >= len(t.s.peers) {
		t.cursor = max(0, len(t.s.peers)-1)
	}
}

func (t *tui) Init() tea.Cmd { return nil }

func (t *tui) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		t.w, t.h = msg.Width, msg.Height
		t.sync()
	case refreshMsg:
		t.sync()
		if t.s.done {
			return t, tea.Quit
		}
	case tea.KeyMsg:
		cmd := t.key(msg)
		t.sync()
		return t, cmd
	}
	return t, nil
}

func (t *tui) key(k tea.KeyMsg) tea.Cmd {
	if t.prompt != nil {
		return t.edit(k)
	}
	if len(t.s.pending) > 0 {
		switch k.String() {
		case "y", "n":
			t.e.answer(group(t.s.pending), k.String() == "y")
			return nil
		}
	}
	switch k.Type {
	case tea.KeyPgUp:
		t.scroll(-t.logRoom())
		return nil
	case tea.KeyPgDown:
		t.scroll(t.logRoom())
		return nil
	case tea.KeyHome:
		t.scroll(-len(t.s.log))
		return nil
	case tea.KeyEnd:
		t.follow = true
		return nil
	}
	switch k.String() {
	case "ctrl+c", "q":
		return tea.Quit
	case "up":
		t.cursor = max(0, t.cursor-1)
	case "down":
		if len(t.s.peers) > 0 {
			t.cursor = min(len(t.s.peers)-1, t.cursor+1)
		}
	case "s":
		if _, ok := t.peer(); !ok {
			t.e.note("Nobody to send to yet")
			return nil
		}
		t.ask("Send path", true, func(t *tui, path string) { t.sendPath(path) })
	case "m":
		if _, ok := t.peer(); !ok {
			t.e.note("Nobody to send to yet")
			return nil
		}
		t.ask("Note", false, func(t *tui, text string) { t.sendNote(text) })
	case "j":
		t.ask("Room code to join", false, func(t *tui, code string) {
			if code != "" {
				t.e.join(strings.ToUpper(code))
			}
		})
	case "c":
		t.e.createCode()
	case "x":
		t.e.cancel()
	}
	return nil
}

func (t *tui) edit(k tea.KeyMsg) tea.Cmd {
	p := t.prompt
	p.hint = ""
	switch k.Type {
	case tea.KeyCtrlC:
		return tea.Quit
	case tea.KeyEsc:
		t.prompt = nil
	case tea.KeyEnter:
		t.prompt = nil
		p.done(t, strings.TrimSpace(string(p.buf)))
	case tea.KeyBackspace:
		if n := len(p.buf); n > 0 {
			p.buf = p.buf[:n-1]
		}
	case tea.KeyCtrlU:
		p.buf = p.buf[:0]
	case tea.KeyCtrlW:
		p.buf = trimWord(p.buf)
	case tea.KeyTab:
		if p.path {
			var more []string
			s := expandHome(string(p.buf))
			s, more = complete(s)
			p.buf = []rune(s)
			p.hint = strings.Join(more, "  ")
		}
	case tea.KeySpace:
		p.buf = append(p.buf, ' ')
	case tea.KeyRunes:
		p.buf = append(p.buf, k.Runes...)
	}
	return nil
}

func trimWord(buf []rune) []rune {
	isSep := func(r rune) bool { return r == ' ' || r == '/' || r == filepath.Separator }
	n := len(buf)
	for n > 0 && isSep(buf[n-1]) {
		n--
	}
	for n > 0 && !isSep(buf[n-1]) {
		n--
	}
	return buf[:n]
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + p[1:]
		}
	}
	return p
}

func complete(input string) (string, []string) {
	pattern := input + "*"
	if runtime.GOOS != "windows" {
		pattern = globEscape(input) + "*"
	}
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return input, nil
	}
	_, base := filepath.Split(input)
	hideDots := !strings.HasPrefix(base, ".")
	sep := string(filepath.Separator)
	out := matches[:0]
	for _, m := range matches {
		if hideDots && strings.HasPrefix(filepath.Base(m), ".") {
			continue
		}
		if fi, err := os.Stat(m); err == nil && fi.IsDir() {
			m += sep
		}
		out = append(out, m)
	}
	switch len(out) {
	case 0:
		return input, nil
	case 1:
		return out[0], nil
	}
	lcp := commonPrefix(out)
	if len(lcp) > len(input) {
		return lcp, nil
	}
	names := make([]string, len(out))
	for i, m := range out {
		names[i] = filepath.Base(strings.TrimSuffix(m, sep))
		if strings.HasSuffix(m, sep) {
			names[i] += sep
		}
	}
	return input, names
}

func globEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`*?[\`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func commonPrefix(ss []string) string {
	p := []rune(ss[0])
	for _, s := range ss[1:] {
		r := []rune(s)
		n := 0
		for n < len(p) && n < len(r) && p[n] == r[n] {
			n++
		}
		p = p[:n]
	}
	return string(p)
}

func (t *tui) ask(label string, path bool, done func(*tui, string)) {
	t.prompt = &prompt{label: label, path: path, done: done}
}

func (t *tui) scroll(delta int) {
	room := t.logRoom()
	bottom := max(0, len(t.s.log)-room)
	top := t.logTop
	if t.follow {
		top = bottom
	}
	top = min(max(0, top+delta), bottom)
	t.logTop = top
	t.follow = top >= bottom
}

func (t *tui) peer() (protocol.Peer, bool) {
	if t.cursor < 0 || t.cursor >= len(t.s.peers) {
		return protocol.Peer{}, false
	}
	return t.s.peers[t.cursor], true
}

func (t *tui) sendPath(path string) {
	peer, ok := t.peer()
	if !ok {
		t.e.note("Nobody to send to")
		return
	}
	if path == "" {
		return
	}
	files, skipped, err := client.Expand(expandHome(path))
	if err != nil {
		t.e.note("%v", err)
		return
	}
	if skipped > 0 {
		t.e.note("Skipping %d empty or special files", skipped)
	}
	if len(files) == 0 {
		t.e.note("Nothing to send in %s", path)
		return
	}
	t.e.send(peer, files)
}

func (t *tui) sendNote(text string) {
	peer, ok := t.peer()
	switch {
	case !ok:
		t.e.note("Nobody to send to")
		return
	case text == "":
		return
	case len(text) > protocol.MaxSnippetBytes:
		t.e.note("The note is longer than %d bytes", protocol.MaxSnippetBytes)
		return
	}
	t.e.sendNote(peer, text)
}

var (
	dim   = lipgloss.NewStyle().Faint(true)
	head  = lipgloss.NewStyle().Bold(true)
	sel   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	alert = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))
	good  = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	bad   = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
)

func (t *tui) View() string {
	s := &t.s
	var lines []string
	title := fmt.Sprintf("beam  %s %s  ·  %s  ·  %s", s.self.Emoji, clean(s.self.Name), t.e.o.server, s.status)
	if s.code != "" {
		title += "  ·  room " + s.code
		if s.codeTTL != "" {
			title += " (" + s.codeTTL + " idle)"
		}
	}
	lines = append(lines, head.Render(t.fit(title)))

	lines = append(lines, t.rule("Devices"))
	if len(s.peers) == 0 {
		lines = append(lines, dim.Render(t.fit("  nobody else here yet — open beam on another device, or press c for a room code")))
	}
	for i, p := range s.peers {
		name := p.Emoji + " " + clean(p.Name)
		if i == t.cursor {
			lines = append(lines, t.fit(sel.Render("› "+name)+"  "+dim.Render(p.Device)))
		} else {
			lines = append(lines, t.fit("  "+name+"  "+dim.Render(p.Device)))
		}
	}

	if len(s.rows) > 0 {
		lines = append(lines, t.rule("Transfers"))
		for i := range s.rows {
			lines = append(lines, t.rowLine(&s.rows[i]))
		}
		if extra := t.extraLine(); extra != "" {
			lines = append(lines, dim.Render(t.fit(extra)))
		}
	}

	room := t.logRoom()
	top := t.logTop
	if t.follow {
		top = max(0, len(s.log)-room)
	}
	label := "Log"
	if !t.follow {
		label = fmt.Sprintf("Log · %d newer below · End to follow", len(s.log)-min(len(s.log), top+room))
	}
	lines = append(lines, t.rule(label))
	shown := s.log[top:min(len(s.log), top+room)]
	for _, l := range shown {
		lines = append(lines, dim.Render(t.fit("  "+l)))
	}
	for i := len(shown); i < room; i++ {
		lines = append(lines, "")
	}
	lines = append(lines, t.footer()...)
	return strings.Join(lines, "\n")
}

func (t *tui) fit(s string) string { return ansi.Truncate(s, t.w, "…") }

func (t *tui) rule(label string) string {
	line := "── " + label + " "
	if pad := t.w - lipgloss.Width(line); pad > 0 {
		line += strings.Repeat("─", pad)
	}
	return dim.Render(t.fit(line))
}

func (t *tui) maxRows() int { return max(2, t.h/3) }

func (t *tui) extraLine() string {
	var extra []string
	if t.s.more > 0 {
		extra = append(extra, fmt.Sprintf("… %d more", t.s.more))
	}
	if t.s.queued > 0 {
		extra = append(extra, fmt.Sprintf("%d queued", t.s.queued))
	}
	if len(extra) == 0 {
		return ""
	}
	return "  " + strings.Join(extra, " · ")
}

func (t *tui) rowLine(r *row) string {
	arrow, join := "↓", "←"
	if r.out {
		arrow, join = "↑", "→"
	}
	var right string
	switch r.State {
	case client.Done:
		right = good.Render("✓ done") + "  " + client.HumanBytes(r.Size)
	case client.Failed:
		reason := "failed"
		if r.Err != nil {
			reason = r.Err.Error()
		}
		right = bad.Render("✗ " + clean(reason))
	case client.Offered:
		right = dim.Render("waiting for an answer") + "  " + client.HumanBytes(r.Size)
	default:
		width := 10
		if t.w >= 100 {
			width = 20
		}
		frac := 0.0
		if r.Size > 0 {
			frac = float64(r.Done) / float64(r.Size)
		}
		right = fmt.Sprintf("%s %3d%%  %s/%s", bar(width, frac), int(frac*100), client.HumanBytes(r.Done), client.HumanBytes(r.Size))
		if r.State == client.Paused {
			right += "  " + dim.Render("paused")
		} else {
			right += fmt.Sprintf("  %s/s  eta %s", client.HumanBytes(int64(r.rate)), eta(r.rate, r.Size-r.Done))
		}
	}
	if r.SAS != "" && !r.State.Terminal() {
		right += "  " + alert.Render(r.SAS)
	}
	left := fmt.Sprintf("  %s %s %s %s", arrow, r.Label, join, clean(r.Peer.Name))
	if room := t.w - lipgloss.Width(right) - 2; room > 8 {
		left = ansi.Truncate(left, room, "…")
	}
	return t.fit(left + "  " + right)
}

func bar(width int, frac float64) string {
	fill := int(frac*float64(width) + 0.5)
	fill = min(max(fill, 0), width)
	return strings.Repeat("█", fill) + strings.Repeat("░", width-fill)
}

func (t *tui) logRoom() int {
	used := 2 + max(1, len(t.s.peers)) + 1 + len(t.footer())
	if n := len(t.s.rows); n > 0 {
		used += 1 + n
		if t.extraLine() != "" {
			used++
		}
	}
	return max(1, t.h-used)
}

func (t *tui) footer() []string {
	if p := t.prompt; p != nil {
		lines := []string{t.fit(head.Render(p.label+": ") + string(p.buf) + "█")}
		if p.hint != "" {
			lines = append(lines, dim.Render(t.fit("  "+p.hint)))
		}
		return lines
	}
	if len(t.s.pending) > 0 {
		q := strings.TrimSpace(question(group(t.s.pending)))
		q = q[strings.LastIndex(q, "\n")+1:]
		return []string{alert.Render(t.fit(clean(q)))}
	}
	return []string{dim.Render(t.fit("↑↓ pick · s send · m note · c room code · j join · x cancel · PgUp/PgDn log · q quit"))}
}
