package client

import (
	"crypto/cipher"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/TamerlanK/beam/internal/protocol"
	"github.com/google/uuid"
)

const maxInflight = 24

type State int

const (
	Queued State = iota
	Offered
	Active
	Paused
	Done
	Failed
)

func terminal(s State) bool { return s == Done || s == Failed }

func (s State) Terminal() bool { return terminal(s) }

func (s State) String() string {
	switch s {
	case Queued:
		return "queued"
	case Offered:
		return "offered"
	case Active:
		return "active"
	case Paused:
		return "paused"
	case Done:
		return "done"
	case Failed:
		return "failed"
	}
	return "unknown"
}

type Status struct {
	ID    uuid.UUID
	Label string
	Peer  protocol.Peer
	Size  int64
	Done  int64
	SAS   string
	State State
	Err   error
}

var reasons = map[string]string{
	"offer-timeout":         "no answer in 30 seconds",
	"peer-disconnected":     "they disconnected",
	"peer-left-room":        "they left the room",
	"receiver-backpressure": "their connection stalled",
	"sender-backpressure":   "your connection stalled",
	"server-shutdown":       "the server shut down",
}

func reasonText(r string) string {
	if t := reasons[r]; t != "" {
		return t
	}
	return r
}

type File struct {
	Path string
	Name string
	Dir  string
	Size int64
	Mime string
}

func (f File) Label() string { return path.Join(f.Dir, f.Name) }

func Stat(path string) (File, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return File{}, err
	}
	name := filepath.Base(path)
	switch {
	case fi.IsDir():
		return File{}, fmt.Errorf("%s is a directory", path)
	case fi.Size() == 0:
		return File{}, fmt.Errorf("%s is empty", path)
	case fi.Size() > protocol.MaxDeclaredSize:
		return File{}, fmt.Errorf("%s is larger than 50GB", path)
	case len(name) > protocol.MaxFilenameBytes:
		return File{}, fmt.Errorf("%s: name longer than 255 bytes", path)
	}
	m, _, _ := mime.ParseMediaType(mime.TypeByExtension(filepath.Ext(name)))
	return File{Path: path, Name: name, Size: fi.Size(), Mime: m}, nil
}

func Expand(root string) (files []File, skipped int, err error) {
	fi, err := os.Stat(root)
	if err != nil {
		return nil, 0, err
	}
	if !fi.IsDir() {
		f, err := Stat(root)
		return []File{f}, 0, err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, 0, err
	}
	top := filepath.Base(abs)
	if top == "" || top == "." || strings.ContainsAny(top, `/\:`) {
		top = "folder"
	}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := os.Stat(p)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			skipped++
			return nil
		}
		f, err := Stat(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		f.Dir = path.Join(top, filepath.ToSlash(filepath.Dir(rel)))
		if _, ok := protocol.SplitPath(f.Dir); !ok {
			return fmt.Errorf("%s: folder path too long or not sendable", p)
		}
		files = append(files, f)
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	if len(files) == 0 {
		return nil, 0, fmt.Errorf("%s has no files to send", root)
	}
	return files, skipped, nil
}

type sendXfer struct {
	id      uuid.UUID
	f       File
	file    *os.File
	kp      keypair
	commit  string
	key     cipher.AEAD
	sas     string
	state   State
	offset  int64
	credits int
	since   time.Time
	err     error
}

type Sender struct {
	c     *Client
	to    string
	batch *protocol.Batch
	xfers []*sendXfer
	byID  map[uuid.UUID]*sendXfer
	frame []byte

	OnStart    func(f File, sas string)
	OnPause    func(f File)
	OnProgress func()
	OnDone     func(f File, err error)
}

func NewSender(c *Client, to string, files []File) *Sender {
	s := &Sender{c: c, to: to, byID: map[uuid.UUID]*sendXfer{}, frame: make([]byte, protocol.MaxFrameSize)}
	var total int64
	for _, f := range files {
		x := &sendXfer{id: uuid.New(), f: f}
		s.xfers = append(s.xfers, x)
		s.byID[x.id] = x
		total += f.Size
	}
	if len(files) > 1 {
		s.batch = &protocol.Batch{ID: uuid.NewString(), Files: len(files), Bytes: total}
	}
	return s
}

func (s *Sender) Start() { s.fill() }

func (s *Sender) fill() {
	live := 0
	for _, x := range s.xfers {
		if x.state == Offered || x.state == Active || x.state == Paused {
			live++
		}
	}
	for _, x := range s.xfers {
		if live >= maxInflight {
			return
		}
		if x.state == Queued {
			s.offer(x)
			live++
		}
	}
}

func (s *Sender) offer(x *sendXfer) {
	if x.file == nil {
		f, err := os.Open(x.f.Path)
		if err != nil {
			s.finish(x, err)
			return
		}
		x.file = f
	}
	if x.kp.pub == "" {
		kp, err := newKeypair()
		if err == nil {
			x.commit, err = commitment(kp.pub)
		}
		if err != nil {
			s.finish(x, err)
			return
		}
		x.kp = kp
	}
	var hint int64
	if x.offset > 0 {
		hint = min(x.offset, (x.f.Size-1)/protocol.ChunkSize*protocol.ChunkSize)
	}
	x.state, x.since, x.credits = Offered, time.Now(), 0
	err := s.c.Send(protocol.TypeTransferOffer, protocol.TransferOffer{
		ID: x.id.String(), To: s.to, Name: x.f.Name, Path: x.f.Dir, Size: x.f.Size, Mime: x.f.Mime, Key: x.commit, Offset: hint, Batch: s.batch,
	})
	if err != nil {
		x.state = Paused
	}
}

func (s *Sender) reoffer() {
	if _, ok := s.c.Peer(s.to); !ok {
		return
	}
	for _, x := range s.xfers {
		if x.state == Paused {
			s.offer(x)
		}
	}
}

func (s *Sender) parse(data json.RawMessage, v any, id *string) *sendXfer {
	if json.Unmarshal(data, v) != nil {
		return nil
	}
	u, err := uuid.Parse(*id)
	if err != nil {
		return nil
	}
	return s.byID[u]
}

func (s *Sender) Handle(ev Event) {
	switch ev.Type {
	case Disconnected:
		for _, x := range s.xfers {
			s.pause(x)
		}
	case Connected:
		s.reoffer()
	case protocol.TypePeerJoined:
		var p protocol.Peer
		if json.Unmarshal(ev.Data, &p) == nil && p.ID == s.to {
			s.reoffer()
		}
	case protocol.TypeTransferAnswer:
		var a protocol.TransferAnswer
		if x := s.parse(ev.Data, &a, &a.ID); x != nil && x.state == Offered {
			s.answered(x, a)
		}
	case protocol.TypeFlowCredit:
		var m protocol.FlowCredit
		if x := s.parse(ev.Data, &m, &m.ID); x != nil && x.state == Active {
			x.credits += m.N
			s.pump(x)
		}
	case protocol.TypeTransferComplete:
		var m protocol.TransferComplete
		if x := s.parse(ev.Data, &m, &m.ID); x != nil && x.state == Active {
			s.finish(x, nil)
		}
	case protocol.TypeTransferFailed:
		var m protocol.TransferFailed
		x := s.parse(ev.Data, &m, &m.ID)
		if x == nil || terminal(x.state) {
			return
		}
		if m.Reason == "peer-disconnected" {
			s.pause(x)
			return
		}
		s.finish(x, errors.New(reasonText(m.Reason)))
	case protocol.TypeTransferCancel:
		var m protocol.TransferCancel
		if x := s.parse(ev.Data, &m, &m.ID); x != nil && !terminal(x.state) {
			s.finish(x, errors.New("canceled by the receiver"))
		}
	}
}

func (s *Sender) answered(x *sendXfer, a protocol.TransferAnswer) {
	if !a.Accept {
		if s.batch != nil {
			for _, y := range s.xfers {
				if y.state == Queued {
					y.state, y.err = Failed, errors.New("declined")
				}
			}
		}
		s.finish(x, errors.New("declined"))
		return
	}

	x.key, x.sas = nil, ""
	if a.Key != "" {
		key, sas, err := shared(x.kp, a.Key)
		if err != nil {
			s.cancel(x, "key exchange failed")
			return
		}
		x.key, x.sas = key, sas
		if err := s.c.Send(protocol.TypeTransferKey, protocol.TransferKey{ID: x.id.String(), Key: x.kp.pub}); err != nil {
			return
		}
	}
	if a.Offset < 0 || a.Offset > x.offset || a.Offset%protocol.ChunkSize != 0 {
		s.cancel(x, "bad resume offset")
		return
	}
	x.offset, x.credits, x.state = a.Offset, protocol.CreditWindow, Active
	if s.OnStart != nil {
		s.OnStart(x.f, x.sas)
	}
	s.pump(x)
}

func (s *Sender) pump(x *sendXfer) {
	for x.state == Active && x.credits > 0 && x.offset < x.f.Size {
		n := min(int64(protocol.ChunkSize), x.f.Size-x.offset)
		buf := s.frame[protocol.FrameOverhead : protocol.FrameOverhead+n]
		if _, err := x.file.ReadAt(buf, x.offset); err != nil {
			s.cancel(x, "couldn't read the file")
			return
		}
		payload := buf
		if x.key != nil {
			payload = sealChunk(x.key, uint32(x.offset/protocol.ChunkSize), x.id, buf[:0], buf)
		}
		copy(s.frame, x.id[:])
		if err := s.c.SendFrame(s.frame[:protocol.FrameOverhead+len(payload)]); err != nil {
			return
		}
		x.credits--
		x.offset += n
		if s.OnProgress != nil {
			s.OnProgress()
		}
	}
}

func (s *Sender) pause(x *sendXfer) {
	if x.state == Offered || x.state == Active {
		x.state, x.credits, x.since = Paused, 0, time.Now()
		if s.OnPause != nil {
			s.OnPause(x.f)
		}
	}
}

func (s *Sender) cancel(x *sendXfer, reason string) {
	if x.state == Offered || x.state == Active {
		_ = s.c.Send(protocol.TypeTransferCancel, protocol.TransferCancel{ID: x.id.String(), Reason: reason})
	}
	s.finish(x, errors.New(reason))
}

func (s *Sender) finish(x *sendXfer, err error) {
	if x.file != nil {
		_ = x.file.Close()
		x.file = nil
	}
	x.state, x.err = Done, nil
	if err != nil {
		x.state, x.err = Failed, err
	}
	if s.OnDone != nil {
		s.OnDone(x.f, err)
	}
	s.fill()
}

func (s *Sender) Reap(maxWait time.Duration) {
	if maxWait <= 0 {
		return
	}
	for _, x := range s.xfers {
		if x.state == Paused && time.Since(x.since) > maxWait {
			s.finish(x, errors.New("they didn't come back"))
		}
	}
}

func (s *Sender) Cancel() {
	for _, x := range s.xfers {
		if !terminal(x.state) {
			s.cancel(x, "canceled")
		}
	}
}

func (s *Sender) Finished() bool {
	for _, x := range s.xfers {
		if !terminal(x.state) {
			return false
		}
	}
	return true
}

func (s *Sender) Failed() int {
	n := 0
	for _, x := range s.xfers {
		if x.state == Failed {
			n++
		}
	}
	return n
}

func (s *Sender) Xfers() []Status {
	peer := protocol.Peer{ID: s.to}
	if s.c != nil {
		if p, ok := s.c.Peer(s.to); ok {
			peer = p
		}
	}
	out := make([]Status, 0, len(s.xfers))
	for _, x := range s.xfers {
		done := x.offset
		if x.state == Done {
			done = x.f.Size
		}
		out = append(out, Status{ID: x.id, Label: x.f.Label(), Peer: peer, Size: x.f.Size, Done: done, SAS: x.sas, State: x.state, Err: x.err})
	}
	return out
}

func (s *Sender) Progress() (sent, total int64) {
	for _, x := range s.xfers {
		total += x.f.Size
		if x.state == Done {
			sent += x.f.Size
		} else {
			sent += x.offset
		}
	}
	return sent, total
}

type Offer struct {
	ID        uuid.UUID
	Name      string
	Dir       string
	Size      int64
	From      protocol.Peer
	Encrypted bool
	Batch     *protocol.Batch
	commit    string
}

func (o *Offer) Label() string { return path.Join(o.Dir, o.Name) }

const batchTTL = 10 * time.Minute

type decision struct {
	from   string
	accept bool
	at     time.Time
}

type recvXfer struct {
	seq    int
	id     uuid.UUID
	name   string
	dir    string
	size   int64
	from   protocol.Peer
	commit string
	kp     keypair
	key    cipher.AEAD
	sas    string
	part   *os.File
	path   string
	bytes  int64
	state  State
	since  time.Time
	err    error
}

type Receiver struct {
	c       *Client
	dir     string
	pending []*Offer
	xfers   map[uuid.UUID]*recvXfer
	batches map[string]decision
	seq     int

	OnStart    func(name string, from protocol.Peer, sas string)
	OnPause    func(name string, from protocol.Peer)
	OnProgress func()
	OnDone     func(name string, from protocol.Peer, path string, err error)
	OnNote     func(from protocol.Peer, text string)
	OnExpired  func(o *Offer, reason string)
}

func NewReceiver(c *Client, dir string) *Receiver {
	return &Receiver{c: c, dir: dir, xfers: map[uuid.UUID]*recvXfer{}, batches: map[string]decision{}}
}

func (x *recvXfer) label() string { return path.Join(x.dir, x.name) }

func (r *Receiver) Pending() []*Offer { return slices.Clone(r.pending) }

func (r *Receiver) Active() int {
	n := 0
	for _, x := range r.xfers {
		if x.state == Active || x.state == Paused {
			n++
		}
	}
	return n
}

func (r *Receiver) Xfers() []Status {
	out := make([]Status, 0, len(r.xfers))
	for _, x := range r.xfers {
		out = append(out, Status{ID: x.id, Label: x.label(), Peer: x.from, Size: x.size, Done: x.bytes, SAS: x.sas, State: x.state, Err: x.err})
	}
	sort.Slice(out, func(i, j int) bool { return r.xfers[out[i].ID].seq < r.xfers[out[j].ID].seq })
	return out
}

func (r *Receiver) Progress() (got, total int64) {
	for _, x := range r.xfers {
		if x.state == Active || x.state == Paused {
			got += x.bytes
			total += x.size
		}
	}
	return got, total
}

func (r *Receiver) pause(x *recvXfer) {
	if x.state != Active {
		return
	}
	x.state, x.since = Paused, time.Now()
	if r.OnPause != nil {
		r.OnPause(x.label(), x.from)
	}
}

func (r *Receiver) takePending(id uuid.UUID) *Offer {
	for i, o := range r.pending {
		if o.ID == id {
			r.pending = slices.Delete(r.pending, i, i+1)
			return o
		}
	}
	return nil
}

func (r *Receiver) expire(o *Offer, reason string) {
	if r.OnExpired != nil {
		r.OnExpired(o, reason)
	}
}

func (r *Receiver) Handle(ev Event) {
	switch ev.Type {
	case Frame:
		r.chunk(ev.Frame)
	case Disconnected:
		for _, o := range r.pending {
			r.expire(o, "connection lost")
		}
		r.pending = nil
		for _, x := range r.xfers {
			r.pause(x)
		}
	case protocol.TypeTransferOffer:
		r.offered(ev.Data)
	case protocol.TypeTransferKey:
		var m protocol.TransferKey
		if json.Unmarshal(ev.Data, &m) != nil {
			return
		}
		id, _ := uuid.Parse(m.ID)
		x := r.xfers[id]
		if x == nil || x.state != Active || x.commit == "" {
			return
		}
		c, err := commitment(m.Key)
		if err != nil || c != x.commit {
			r.fail(x, "key verification failed")
			return
		}
		key, sas, err := shared(x.kp, m.Key)
		if err != nil {
			r.fail(x, "key exchange failed")
			return
		}
		x.key, x.sas = key, sas
		if r.OnStart != nil {
			r.OnStart(x.label(), x.from, sas)
		}
	case protocol.TypeTransferFailed:
		var m protocol.TransferFailed
		if json.Unmarshal(ev.Data, &m) != nil {
			return
		}
		id, _ := uuid.Parse(m.ID)
		if o := r.takePending(id); o != nil {
			r.expire(o, reasonText(m.Reason))
			return
		}
		x := r.xfers[id]
		if x == nil || terminal(x.state) {
			return
		}
		if m.Reason == "peer-disconnected" {
			r.pause(x)
			return
		}
		r.abandon(x, errors.New(reasonText(m.Reason)))
	case protocol.TypeTransferCancel:
		var m protocol.TransferCancel
		if json.Unmarshal(ev.Data, &m) != nil {
			return
		}
		id, _ := uuid.Parse(m.ID)
		if o := r.takePending(id); o != nil {
			r.expire(o, "withdrawn by the sender")
			return
		}
		if x := r.xfers[id]; x != nil && !terminal(x.state) {
			r.abandon(x, errors.New("canceled by the sender"))
		}
	case protocol.TypeSnippet:
		var m protocol.Snippet
		if json.Unmarshal(ev.Data, &m) == nil && m.From != nil && r.OnNote != nil {
			r.OnNote(*m.From, m.Text)
		}
	}
}

func (r *Receiver) offered(data json.RawMessage) {
	var m protocol.TransferOffer
	if json.Unmarshal(data, &m) != nil || m.From == nil || m.Size <= 0 {
		return
	}
	id, err := uuid.Parse(m.ID)
	if err != nil {
		return
	}
	name, dir := safeName(m.Name), safePath(m.Path)
	if x := r.xfers[id]; x != nil {
		switch {
		case (x.state == Active || x.state == Paused) && x.from.ID == m.From.ID && x.name == name && x.dir == dir && x.size == m.Size:
			r.resume(x, m)
		case x.state == Done:

			_ = r.c.Send(protocol.TypeTransferAnswer, protocol.TransferAnswer{ID: m.ID})
		}
		return
	}
	for _, o := range r.pending {
		if o.ID == id {
			return
		}
	}
	o := &Offer{ID: id, Name: name, Dir: dir, Size: m.Size, From: *m.From, Encrypted: m.Key != "", Batch: m.Batch, commit: m.Key}
	r.pending = append(r.pending, o)
	if m.Batch != nil {
		if d, ok := r.batches[m.Batch.ID]; ok && d.from == m.From.ID && time.Since(d.at) < batchTTL {
			_ = r.Answer(o, d.accept)
		}
	}
}

func (r *Receiver) resume(x *recvXfer, m protocol.TransferOffer) {
	off := min(x.bytes/protocol.ChunkSize*protocol.ChunkSize, m.Offset)
	if err := x.part.Truncate(off); err != nil {
		r.fail(x, "couldn't reopen the part file")
		return
	}
	x.bytes = off
	if m.Key != x.commit {
		x.commit, x.key, x.sas = m.Key, nil, ""
	}
	pub := ""
	if x.commit != "" {
		if x.kp.pub == "" {
			kp, err := newKeypair()
			if err != nil {
				r.fail(x, "couldn't set up encryption")
				return
			}
			x.kp = kp
		}
		pub = x.kp.pub
	}
	x.state = Active
	err := r.c.Send(protocol.TypeTransferAnswer, protocol.TransferAnswer{ID: m.ID, Accept: true, Key: pub, Offset: off})
	if err != nil {
		r.pause(x)
		return
	}
	if x.commit == "" && r.OnStart != nil {
		r.OnStart(x.label(), x.from, "")
	}
}

var ErrExpired = errors.New("the offer is no longer open")

func (r *Receiver) Answer(o *Offer, accept bool) error {
	i := slices.Index(r.pending, o)
	if i < 0 {
		return ErrExpired
	}
	r.pending = slices.Delete(r.pending, i, i+1)
	if o.Batch != nil {
		r.batches[o.Batch.ID] = decision{from: o.From.ID, accept: accept, at: time.Now()}
	}
	if !accept {
		return r.c.Send(protocol.TypeTransferAnswer, protocol.TransferAnswer{ID: o.ID.String()})
	}
	x := &recvXfer{seq: r.seq, id: o.ID, name: o.Name, dir: o.Dir, size: o.Size, from: o.From, commit: o.commit, state: Active}
	r.seq++
	x.path = filepath.Join(r.dir, ".beam-"+o.ID.String()+".part")
	f, err := os.OpenFile(x.path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		_ = r.c.Send(protocol.TypeTransferAnswer, protocol.TransferAnswer{ID: o.ID.String()})
		return err
	}
	x.part = f
	pub := ""
	if x.commit != "" {
		if x.kp, err = newKeypair(); err != nil {
			_ = f.Close()
			_ = os.Remove(x.path)
			return err
		}
		pub = x.kp.pub
	}
	r.xfers[o.ID] = x
	if err := r.c.Send(protocol.TypeTransferAnswer, protocol.TransferAnswer{ID: o.ID.String(), Accept: true, Key: pub}); err != nil {
		r.pause(x)
		return nil
	}
	if x.commit == "" && r.OnStart != nil {
		r.OnStart(x.label(), x.from, "")
	}
	return nil
}

func (r *Receiver) chunk(frame []byte) {
	id, data, err := protocol.SplitFrame(frame)
	if err != nil {
		return
	}
	x := r.xfers[id]
	if x == nil || x.state != Active {
		return
	}
	if x.commit != "" && x.key == nil {
		r.fail(x, "chunk before key")
		return
	}
	plain := data
	if x.key != nil {
		if plain, err = openChunk(x.key, uint32(x.bytes/protocol.ChunkSize), id, data[:0], data); err != nil {
			r.fail(x, "integrity check failed")
			return
		}
	}
	if x.bytes+int64(len(plain)) > x.size {
		r.fail(x, "more bytes than announced")
		return
	}
	if _, err := x.part.WriteAt(plain, x.bytes); err != nil {
		r.fail(x, "couldn't write the file")
		return
	}
	x.bytes += int64(len(plain))
	if r.OnProgress != nil {
		r.OnProgress()
	}
	if x.bytes == x.size {
		r.finish(x)
	}
}

func (r *Receiver) finish(x *recvXfer) {
	err := x.part.Sync()
	if cerr := x.part.Close(); err == nil {
		err = cerr
	}
	x.part = nil
	dir := filepath.Join(r.dir, filepath.FromSlash(x.dir))
	if err == nil {
		err = os.MkdirAll(dir, 0o755)
	}
	var final string
	if err == nil {
		final, err = uniquePath(dir, x.name)
	}
	if err == nil {
		err = os.Rename(x.path, final)
	}
	if err != nil {
		_ = os.Remove(x.path)
		x.state, x.err = Failed, fmt.Errorf("couldn't save the file: %w", err)
		if r.OnDone != nil {
			r.OnDone(x.label(), x.from, "", x.err)
		}
		return
	}
	x.state = Done
	if r.OnDone != nil {
		r.OnDone(x.label(), x.from, final, nil)
	}
}

func (r *Receiver) fail(x *recvXfer, reason string) {
	if x.state == Active {
		_ = r.c.Send(protocol.TypeTransferCancel, protocol.TransferCancel{ID: x.id.String(), Reason: reason})
	}
	r.abandon(x, errors.New(reason))
}

func (r *Receiver) abandon(x *recvXfer, err error) {
	if x.part != nil {
		_ = x.part.Close()
		x.part = nil
	}
	_ = os.Remove(x.path)
	x.state, x.err = Failed, err
	if r.OnDone != nil {
		r.OnDone(x.label(), x.from, "", err)
	}
}

func (r *Receiver) Reap(maxWait time.Duration) {
	if maxWait <= 0 {
		return
	}
	for _, x := range r.xfers {
		if x.state == Paused && time.Since(x.since) > maxWait {
			r.abandon(x, errors.New("the sender didn't come back"))
		}
	}
}

func (r *Receiver) Cancel() {
	for _, o := range r.pending {
		_ = r.c.Send(protocol.TypeTransferAnswer, protocol.TransferAnswer{ID: o.ID.String()})
	}
	r.pending = nil
	for _, x := range r.xfers {
		if x.state == Active || x.state == Paused {
			r.fail(x, "canceled")
		}
	}
}

var reservedName = regexp.MustCompile(`(?i)^(con|prn|aux|nul|com[1-9]|lpt[1-9])(\..*)?$`)

func safeName(name string) string {
	name = name[strings.LastIndexAny(name, `/\`)+1:]
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (runtime.GOOS == "windows" && strings.ContainsRune(`<>:"|?*`, r)) {
			return '_'
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if runtime.GOOS == "windows" {
		name = strings.TrimRight(name, ". ")
		if reservedName.MatchString(name) {
			name = "_" + name
		}
	}
	if name == "" || name == "." || name == ".." {
		name = "file"
	}
	return name
}

func safePath(p string) string {
	segs, ok := protocol.SplitPath(p)
	if !ok {
		return ""
	}
	for i, s := range segs {
		segs[i] = safeName(s)
	}
	return strings.Join(segs, "/")
}

func uniquePath(dir, name string) (string, error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 0; i < 1000; i++ {
		cand := name
		if i > 0 {
			cand = fmt.Sprintf("%s (%d)%s", stem, i, ext)
		}
		p := filepath.Join(dir, cand)
		_, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return p, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("too many files named %s", name)
}
