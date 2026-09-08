package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/TamerlanK/beam/internal/client"
	"github.com/TamerlanK/beam/internal/protocol"
)

const usage = `beam drops files between any two devices.

  beam [serve] [flags]         run the server (default)
  beam ls                      list the devices in your room
  beam send [flags] PATH...    send files or whole folders to a device
  beam send -text "..."        send a note to a device
  beam recv [flags]            receive files and folders into a directory

Run "beam <command> -h" for its flags. BEAM_SERVER sets the default server.
`

type opts struct {
	server, name, join string
	share              bool
	wait               time.Duration
}

func (o *opts) flags(fs *flag.FlagSet, share bool) {
	fs.StringVar(&o.server, "server", envOr("BEAM_SERVER", "http://localhost:8080"), "beam server URL")
	fs.StringVar(&o.name, "name", "", "device name to show (default: the saved one)")
	fs.StringVar(&o.join, "join", "", "room code shown on another device")
	if share {
		fs.BoolVar(&o.share, "share", false, "print a room code for devices on other networks")
	}
	fs.DurationVar(&o.wait, "wait", 5*time.Minute, "how long to wait for a device to appear or come back (0 = forever)")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func runCLI(cmd string, args []string) int {
	var o opts
	fs := flag.NewFlagSet("beam "+cmd, flag.ContinueOnError)
	o.flags(fs, cmd != "ls")
	var to, text, dir string
	var yes, once bool
	switch cmd {
	case "send":
		fs.StringVar(&to, "to", "", "device to send to: name or id, or a prefix of one (default: the only other device)")
		fs.StringVar(&text, "text", "", "send this note instead of files")
	case "recv":
		fs.StringVar(&dir, "dir", ".", "directory to save into")
		fs.BoolVar(&yes, "yes", false, "accept every offer without asking")
		fs.BoolVar(&once, "once", false, "exit after the first transfer finishes")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch cmd {
	case "ls":
		err = ls(ctx, &o)
	case "send":
		err = send(ctx, &o, to, text, fs.Args())
	case "recv":
		err = recv(ctx, &o, dir, yes, once)
	}
	status("")
	switch {
	case err == nil:
		return 0
	case ctx.Err() != nil:
		say("interrupted")
		return 130
	default:
		say("beam: %v", err)
		return 1
	}
}

type session struct {
	c      *client.Client
	o      *opts
	cancel context.CancelFunc
	done   chan error
	once   sync.Once
	err    error
	ready  bool
	down   bool
	target protocol.Peer
}

func (o *opts) connect() (*session, error) {
	id, err := client.LoadIdentity()
	if err != nil {
		say("couldn't save this device's identity (%v); peers will see a new device next time", err)
	}
	if o.name != "" {
		id.Name = o.name
	}
	c, err := client.New(client.Config{Server: o.server, Identity: id, Join: o.join, UserAgent: "beam-cli"})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &session{c: c, o: o, cancel: cancel, done: make(chan error, 1)}
	go func() { s.done <- c.Run(ctx) }()
	return s, nil
}

func (s *session) close() error {
	s.cancel()
	s.once.Do(func() { s.err = <-s.done })
	if errors.Is(s.err, context.Canceled) {
		return nil
	}
	return s.err
}

func (s *session) note(ev client.Event) {
	switch ev.Type {
	case client.Connected:
		s.ready = true
		if s.down {
			s.down = false
			say("Reconnected")
		}
		if s.o.share {
			_ = s.c.Send(protocol.TypeRoomCreate, nil)
		}
	case client.Disconnected:
		s.ready, s.down = false, true
		say("Connection lost, reconnecting…")
	case protocol.TypeRoomCreated:
		var m protocol.RoomCreated
		if json.Unmarshal(ev.Data, &m) == nil {
			say("Room code %s: enter it in beam on another device, or run beam with -join %s", m.Code, m.Code)
		}
	case protocol.TypePeerLeft:
		var m protocol.PeerLeft
		if json.Unmarshal(ev.Data, &m) == nil && m.ID == s.target.ID && s.target.ID != "" {
			say("%s went away, waiting for them to come back", s.target.Name)
		}
	case protocol.TypePeerJoined:
		var p protocol.Peer
		if json.Unmarshal(ev.Data, &p) == nil && p.ID == s.target.ID && s.target.ID != "" {
			say("%s is back", p.Name)
		}
	}
}

func serverError(ev client.Event) error {
	if ev.Type != protocol.TypeError {
		return nil
	}
	var e protocol.Error
	if json.Unmarshal(ev.Data, &e) != nil || e.Message == "" {
		return errors.New("the server refused a message")
	}
	return errors.New(e.Message)
}

func ls(ctx context.Context, o *opts) error {
	s, err := o.connect()
	if err != nil {
		return err
	}
	defer func() { _ = s.close() }()
	select {
	case _, ok := <-s.c.Events:
		if !ok {
			return s.close()
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	self := s.c.Self()
	_, _ = fmt.Fprintf(w, "%s %s\t%s\t%s\tthis device\n", self.Emoji, self.Name, self.Device, self.ID)
	for _, p := range s.c.Peers() {
		_, _ = fmt.Fprintf(w, "%s %s\t%s\t%s\t\n", p.Emoji, p.Name, p.Device, p.ID)
	}
	return w.Flush()
}

func send(ctx context.Context, o *opts, to, text string, paths []string) error {
	switch {
	case text == "" && len(paths) == 0:
		return errors.New("nothing to send: name files or folders, or a note with -text")
	case text != "" && len(paths) > 0:
		return errors.New("send files or a note, not both")
	case len(text) > protocol.MaxSnippetBytes:
		return fmt.Errorf("the note is longer than %d bytes", protocol.MaxSnippetBytes)
	}
	var files []client.File
	skipped := 0
	for _, p := range paths {
		fs, n, err := client.Expand(p)
		if err != nil {
			return err
		}
		files = append(files, fs...)
		skipped += n
	}
	if skipped > 0 {
		say("Skipping %d empty or special files", skipped)
	}
	s, err := o.connect()
	if err != nil {
		return err
	}
	defer func() { _ = s.close() }()
	peer, err := s.find(ctx, to)
	if err != nil {
		return err
	}
	s.target = peer
	if text != "" {
		return s.sendNote(ctx, peer, text)
	}

	snd := client.NewSender(s.c, peer.ID, files)
	m := &meter{}
	label := files[0].Name
	if len(files) > 1 {
		label = fmt.Sprintf("%d files", len(files))
		dirs := make([]string, len(files))
		for i, f := range files {
			dirs[i] = f.Dir
		}
		if top := folderOf(dirs); top != "" {
			label = top + "/"
		}
	}
	snd.OnStart = func(f client.File, sas string) {
		if sas == "" {
			say("Sending %s (%s) to %s, not encrypted: their browser has no crypto", f.Label(), client.HumanBytes(f.Size), peer.Name)
		} else {
			say("Sending %s (%s) to %s · verify %s", f.Label(), client.HumanBytes(f.Size), peer.Name, sas)
		}
	}
	declined := false
	snd.OnDone = func(f client.File, err error) {
		switch {
		case err == nil:
			say("Sent %s", f.Label())
		case err.Error() == "declined" && len(files) > 1:
			if !declined {
				declined = true
				say("%s declined", peer.Name)
			}
		default:
			say("%s: %v", f.Label(), err)
		}
	}
	snd.OnProgress = func() {
		sent, total := snd.Progress()
		m.show(sent, total, label)
	}
	snd.Start()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for !snd.Finished() {
		select {
		case ev, ok := <-s.c.Events:
			if !ok {
				snd.Cancel()
				return s.close()
			}
			if err := serverError(ev); err != nil {
				snd.Cancel()
				return err
			}
			s.note(ev)
			snd.Handle(ev)
		case <-tick.C:
			snd.Reap(o.wait)
			sent, total := snd.Progress()
			m.show(sent, total, label)
		case <-ctx.Done():
			snd.Cancel()
			return ctx.Err()
		}
	}
	if n := snd.Failed(); n > 0 {
		return fmt.Errorf("%d of %d files not sent", n, len(files))
	}
	return nil
}

func (s *session) sendNote(ctx context.Context, peer protocol.Peer, text string) error {
	if err := s.c.Send(protocol.TypeSnippet, protocol.Snippet{To: peer.ID, Text: text}); err != nil {
		return err
	}

	grace := time.After(500 * time.Millisecond)
	for {
		select {
		case ev, ok := <-s.c.Events:
			if !ok {
				return s.close()
			}
			if err := serverError(ev); err != nil {
				return err
			}
		case <-grace:
			say("Sent the note to %s", peer.Name)
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (s *session) find(ctx context.Context, to string) (protocol.Peer, error) {
	var timeout <-chan time.Time
	if s.o.wait > 0 {
		t := time.NewTimer(s.o.wait)
		defer t.Stop()
		timeout = t.C
	}
	waiting := false
	for {
		select {
		case ev, ok := <-s.c.Events:
			if !ok {
				return protocol.Peer{}, s.close()
			}
			if err := serverError(ev); err != nil {
				return protocol.Peer{}, err
			}
			s.note(ev)
			if !s.ready {
				continue
			}
			p, err := pick(s.c.Peers(), to)
			if err != nil {
				return protocol.Peer{}, err
			}
			if p != nil {
				return *p, nil
			}
			if !waiting {
				waiting = true
				if to == "" {
					say("Waiting for a device to show up…")
				} else {
					say("Waiting for %q to show up…", to)
				}
			}
		case <-timeout:
			if to == "" {
				return protocol.Peer{}, fmt.Errorf("no device showed up within %s", s.o.wait)
			}
			return protocol.Peer{}, fmt.Errorf("no device matching %q showed up within %s", to, s.o.wait)
		case <-ctx.Done():
			return protocol.Peer{}, ctx.Err()
		}
	}
}

func pick(peers []protocol.Peer, to string) (*protocol.Peer, error) {
	if to == "" {
		switch len(peers) {
		case 0:
			return nil, nil
		case 1:
			return &peers[0], nil
		default:
			return nil, fmt.Errorf("%d devices in the room, pick one with -to: %s", len(peers), describe(peers))
		}
	}
	q := strings.ToLower(to)
	var exact, loose []protocol.Peer
	for _, p := range peers {
		name, id := strings.ToLower(p.Name), strings.ToLower(p.ID)
		switch {
		case name == q || id == q:
			exact = append(exact, p)
		case strings.HasPrefix(name, q) || strings.HasPrefix(id, q):
			loose = append(loose, p)
		}
	}
	if len(exact) == 0 {
		exact = loose
	}
	switch len(exact) {
	case 0:
		return nil, nil
	case 1:
		return &exact[0], nil
	default:
		return nil, fmt.Errorf("%q matches %d devices: %s", to, len(exact), describe(exact))
	}
}

func describe(peers []protocol.Peer) string {
	parts := make([]string, len(peers))
	for i, p := range peers {
		parts[i] = fmt.Sprintf("%s (%s)", p.Name, p.ID[:8])
	}
	return strings.Join(parts, ", ")
}

type decision struct {
	offers []*client.Offer
	accept bool
}

func recv(ctx context.Context, o *opts, dir string, yes, once bool) error {
	if !yes && !isTerminal(os.Stdin) {
		return errors.New("stdin is not a terminal, so nobody can answer offers: pass -yes to accept them all")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	s, err := o.connect()
	if err != nil {
		return err
	}
	defer func() { _ = s.close() }()

	r := client.NewReceiver(s.c, dir)
	m := &meter{}
	var saved, failed int
	r.OnStart = func(name string, from protocol.Peer, sas string) {
		if sas == "" {
			say("Receiving %s from %s, not encrypted: their browser has no crypto", name, from.Name)
		} else {
			say("Receiving %s from %s · verify %s", name, from.Name, sas)
		}
	}
	r.OnPause = func(name string, from protocol.Peer) {
		say("%s: %s went away, waiting for them to come back", name, from.Name)
	}
	r.OnDone = func(name string, from protocol.Peer, path string, err error) {
		if err != nil {
			failed++
			say("%s from %s: %v", name, from.Name, err)
			return
		}
		saved++
		say("Saved %s", path)
		fmt.Println(path)
	}
	r.OnNote = func(from protocol.Peer, text string) {
		say("Note from %s:\n%s", from.Name, text)
	}
	r.OnExpired = func(off *client.Offer, reason string) {
		say("%s from %s: %s", off.Label(), off.From.Name, reason)
	}
	r.OnProgress = func() {
		got, total := r.Progress()
		m.show(got, total, "receiving")
	}

	in := bufio.NewReader(os.Stdin)
	decisions := make(chan decision, 1)
	asking := false
	prompt := func() {
		pend := r.Pending()
		if len(pend) == 0 || asking {
			return
		}
		if yes {
			for _, off := range pend {
				if err := r.Answer(off, true); err != nil {
					say("%s: %v", off.Label(), err)
				}
			}
			return
		}
		var group []*client.Offer
		for _, off := range pend {
			if off.From.ID == pend[0].From.ID {
				group = append(group, off)
			}
		}
		asking = true
		go func() { decisions <- decision{group, ask(in, question(group))} }()
	}

	banner := false
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case ev, ok := <-s.c.Events:
			if !ok {
				r.Cancel()
				return s.close()
			}
			if err := serverError(ev); err != nil {
				say("%v", err)
			}
			s.note(ev)
			if ev.Type == client.Connected && !banner {
				banner = true
				self := s.c.Self()
				say("Receiving into %s as %s %s. Pick this device in beam to send here.", dir, self.Emoji, self.Name)
			}
			r.Handle(ev)
		case d := <-decisions:
			asking = false
			for _, off := range d.offers {
				if err := r.Answer(off, d.accept); err != nil && !errors.Is(err, client.ErrExpired) {
					say("%s: %v", off.Label(), err)
				}
			}
		case <-tick.C:
			r.Reap(o.wait)
			got, total := r.Progress()
			m.show(got, total, "receiving")
		case <-ctx.Done():
			r.Cancel()
			return ctx.Err()
		}
		prompt()
		if once && saved+failed > 0 && r.Active() == 0 && len(r.Pending()) == 0 && !asking {
			break
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d transfers failed", failed)
	}
	return nil
}

func question(group []*client.Offer) string {
	var b strings.Builder
	var total int64
	count := len(group)
	dirs := make([]string, len(group))
	for i, off := range group {
		total += off.Size
		dirs[i] = off.Dir
		if len(group) > 1 && i < 10 {
			_, _ = fmt.Fprintf(&b, "  %s  %s\n", off.Label(), client.HumanBytes(off.Size))
		}
	}
	if batch := group[0].Batch; batch != nil && batch.Files > count {
		count, total = batch.Files, batch.Bytes
	}
	if count > 10 {
		_, _ = fmt.Fprintf(&b, "  … and %d more\n", count-10)
	}
	enc := ""
	if !group[0].Encrypted {
		enc = ", not encrypted"
	}
	what := fmt.Sprintf("%d files", count)
	if count == 1 {
		what = group[0].Label()
	} else if top := folderOf(dirs); top != "" {
		what = fmt.Sprintf("the folder %s (%d files)", top, count)
	}
	_, _ = fmt.Fprintf(&b, "Accept %s (%s%s) from %s? [y/N] ", what, client.HumanBytes(total), enc, group[0].From.Name)
	return b.String()
}

func folderOf(dirs []string) string {
	top := ""
	for _, d := range dirs {
		first, _, _ := strings.Cut(d, "/")
		if first == "" || (top != "" && first != top) {
			return ""
		}
		top = first
	}
	return top
}

var stderrTTY = isTerminal(os.Stderr)

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

var scr struct {
	mu     sync.Mutex
	status string
	prompt string
}

func say(format string, a ...any) {
	scr.mu.Lock()
	defer scr.mu.Unlock()
	if scr.prompt != "" {
		fmt.Fprintln(os.Stderr)
	}
	wipe()
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	draw()
	if scr.prompt != "" {
		fmt.Fprint(os.Stderr, scr.prompt)
	}
}

func status(line string) {
	if !stderrTTY {
		return
	}
	scr.mu.Lock()
	defer scr.mu.Unlock()
	wipe()
	scr.status = line
	draw()
}

func wipe() {
	if scr.status != "" && scr.prompt == "" {
		fmt.Fprint(os.Stderr, "\r", strings.Repeat(" ", utf8.RuneCountInString(scr.status)), "\r")
	}
}

func draw() {
	if scr.status != "" && scr.prompt == "" {
		fmt.Fprint(os.Stderr, scr.status)
	}
}

func ask(in *bufio.Reader, q string) bool {
	scr.mu.Lock()
	wipe()
	fmt.Fprint(os.Stderr, q)
	scr.prompt = q[strings.LastIndex(strings.TrimRight(q, "\n"), "\n")+1:]
	scr.mu.Unlock()
	line, _ := in.ReadString('\n')
	scr.mu.Lock()
	scr.prompt = ""
	draw()
	scr.mu.Unlock()
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}

type meter struct {
	samples []sample
	last    time.Time
}

type sample struct {
	at time.Time
	n  int64
}

func (m *meter) show(done, total int64, label string) {
	now := time.Now()
	if now.Sub(m.last) < 100*time.Millisecond {
		return
	}
	m.last = now
	if total == 0 {
		status("")
		return
	}
	if n := len(m.samples); n > 0 && m.samples[n-1].n > done {
		m.samples = nil
	}
	m.samples = append(m.samples, sample{now, done})
	if len(m.samples) > 30 {
		m.samples = m.samples[1:]
	}
	var rate float64
	if n := len(m.samples); n > 1 {
		first, last := m.samples[0], m.samples[n-1]
		if dt := last.at.Sub(first.at).Seconds(); dt > 0 {
			rate = float64(last.n-first.n) / dt
		}
	}
	eta := "…"
	if rate > 0 {
		eta = (time.Duration(float64(total-done)/rate*float64(time.Second)) + time.Second/2).Round(time.Second).String()
	}
	line := fmt.Sprintf("%3d%%  %s / %s  %s/s  eta %s  %s", done*100/total, client.HumanBytes(done), client.HumanBytes(total), client.HumanBytes(int64(rate)), eta, label)
	if utf8.RuneCountInString(line) > 100 {
		line = string([]rune(line)[:99]) + "…"
	}
	status(line)
}
