package hub

import (
	"context"
	"log/slog"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/TamerlanK/beam/internal/names"
	"github.com/TamerlanK/beam/internal/protocol"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	joinBurst = 5
	joinRate  = 0.5

	maxTransfersPerSender = 32
)

type inbound struct {
	from  *Client
	text  []byte
	frame []byte
	pool  *[]byte
}

type written struct {
	to *Client
	id uuid.UUID
	n  int
}

type bucket struct {
	tokens float64
	last   time.Time
}

type Hub struct {
	log *slog.Logger

	register   chan *Client
	unregister chan *Client
	inbound    chan inbound
	written    chan written
	done       chan struct{}

	clients   map[string]*Client
	rooms     map[string]*Room
	codes     map[string]*Room
	transfers map[uuid.UUID]*Transfer
	buckets   map[string]*bucket
}

func New(log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	return &Hub{
		log:        log,
		register:   make(chan *Client),
		unregister: make(chan *Client),
		inbound:    make(chan inbound, 256),
		written:    make(chan written, 256),
		done:       make(chan struct{}),
		clients:    map[string]*Client{},
		rooms:      map[string]*Room{},
		codes:      map[string]*Room{},
		transfers:  map[uuid.UUID]*Transfer{},
		buckets:    map[string]*bucket{},
	}
}

func (h *Hub) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			h.shutdown()
			return
		case c := <-h.register:
			h.addClient(c)
		case c := <-h.unregister:
			h.removeClient(c)
		case m := <-h.inbound:
			if m.frame != nil {
				h.handleFrame(m)
			} else {
				h.handleText(m.from, m.text)
			}
		case w := <-h.written:
			h.handleWritten(w)
		case now := <-ticker.C:
			h.tick(now)
		}
	}
}

func (h *Hub) deliver(m inbound) bool {
	select {
	case h.inbound <- m:
		return true
	case <-h.done:
		return false
	}
}

func (h *Hub) drop(c *Client) {
	select {
	case h.unregister <- c:
	case <-h.done:
	}
}

func (h *Hub) shutdown() {
	for _, t := range h.transfers {
		if !t.State.Terminal() {
			h.failTransfer(t, "server-shutdown")
		}
	}
	for _, c := range h.clients {
		close(c.send)
	}
	close(h.done)
	h.log.Info("hub stopped", "clients", len(h.clients))
}

func (h *Hub) trySend(c *Client, f outFrame) bool {
	select {
	case c.send <- f:
		return true
	default:
		return false
	}
}

func (h *Hub) sendJSON(c *Client, msgType string, data any) {
	if !h.trySend(c, outFrame{msgType: websocket.TextMessage, data: protocol.MustEncode(msgType, data)}) {
		c.log.Warn("send queue full, dropping control message", "type", msgType)
	}
}

func (h *Hub) sendErr(c *Client, code, msg string) {
	h.sendJSON(c, protocol.TypeError, protocol.Error{Code: code, Message: msg})
}

func (h *Hub) addClient(c *Client) {
	if _, taken := h.clients[c.ID]; taken {
		c.ID = uuid.NewString()
		c.log.Warn("claimed device id already connected, assigned a fresh one", "newClient", c.ID)
	}
	h.clients[c.ID] = c
	key := "ip:" + c.IP
	room := h.rooms[key]
	if room == nil {
		room = newRoom(key)
		h.rooms[key] = room
	}
	h.joinRoom(c, room)
	c.log.Info("client connected", "ip", c.IP, "device", c.Device, "room", room.key)
}

func (h *Hub) joinRoom(c *Client, room *Room) {
	room.clients[c.ID] = c
	c.room = room
	self := c.Peer()
	for _, other := range room.clients {
		if other != c {
			h.sendJSON(other, protocol.TypePeerJoined, self)
		}
	}
	h.sendJSON(c, protocol.TypeRoomState, protocol.RoomState{Self: self, Peers: room.peersExcept(c)})
}

func (h *Hub) leaveRoom(c *Client) {
	room := c.room
	if room == nil {
		return
	}
	delete(room.clients, c.ID)
	c.room = nil
	for _, other := range room.clients {
		h.sendJSON(other, protocol.TypePeerLeft, protocol.PeerLeft{ID: c.ID})
	}
	if len(room.clients) == 0 {
		delete(h.rooms, room.key)
		if room.code != "" {
			delete(h.codes, room.code)
		}
	}
}

func (h *Hub) removeClient(c *Client) {
	if _, ok := h.clients[c.ID]; !ok {
		return
	}
	delete(h.clients, c.ID)
	for _, t := range h.transfers {
		if t.FromID == c.ID || t.ToID == c.ID {
			h.failTransfer(t, "peer-disconnected")
		}
	}
	h.leaveRoom(c)
	close(c.send)
	c.log.Info("client disconnected")
}

func (h *Hub) handleText(c *Client, raw []byte) {
	if _, ok := h.clients[c.ID]; !ok {
		return
	}
	env, err := protocol.Decode(raw)
	if err != nil {
		h.sendErr(c, protocol.ErrCodeBadMessage, err.Error())
		return
	}
	switch env.Type {
	case protocol.TypeRoomCreate:
		h.handleRoomCreate(c)
	case protocol.TypeRoomJoin:
		var m protocol.RoomJoin
		if !h.unmarshal(c, env, &m) {
			return
		}
		h.handleRoomJoin(c, m)
	case protocol.TypeTransferOffer:
		var m protocol.TransferOffer
		if !h.unmarshal(c, env, &m) {
			return
		}
		h.handleOffer(c, m)
	case protocol.TypeTransferAnswer:
		var m protocol.TransferAnswer
		if !h.unmarshal(c, env, &m) {
			return
		}
		h.handleAnswer(c, m)
	case protocol.TypeTransferCancel:
		var m protocol.TransferCancel
		if !h.unmarshal(c, env, &m) {
			return
		}
		h.handleCancel(c, m)
	case protocol.TypeSnippet:
		var m protocol.Snippet
		if !h.unmarshal(c, env, &m) {
			return
		}
		h.handleSnippet(c, m)
	case protocol.TypeProfile:
		var m protocol.Profile
		if !h.unmarshal(c, env, &m) {
			return
		}
		h.handleProfile(c, m)
	case protocol.TypeRTC:
		var m protocol.RTC
		if !h.unmarshal(c, env, &m) {
			return
		}
		h.handleRTC(c, m)
	default:
		h.sendErr(c, protocol.ErrCodeBadMessage, "unexpected message type "+env.Type)
	}
}

func (h *Hub) unmarshal(c *Client, env protocol.Envelope, v any) bool {
	if err := protocol.UnmarshalData(env, v); err != nil {
		h.sendErr(c, protocol.ErrCodeBadMessage, "bad "+env.Type+" payload")
		return false
	}
	return true
}

func (h *Hub) handleRoomCreate(c *Client) {
	room := c.room
	now := time.Now()
	if room.code == "" || now.After(room.codeExpires) {
		if room.code != "" {
			delete(h.codes, room.code)
		}
		code := newRoomCode()
		for h.codes[code] != nil {
			code = newRoomCode()
		}
		room.code = code
		h.codes[code] = room
	}
	room.codeExpires = now.Add(protocol.RoomCodeTTLSec * time.Second)
	h.sendJSON(c, protocol.TypeRoomCreated, protocol.RoomCreated{Code: room.code, ExpiresIn: protocol.RoomCodeTTLSec})
	c.log.Info("room code created", "room", room.key)
}

func (h *Hub) handleRoomJoin(c *Client, m protocol.RoomJoin) {
	if !h.allowJoin(c.IP, time.Now()) {
		h.sendErr(c, protocol.ErrCodeRateLimited, "too many join attempts, slow down")
		return
	}
	code := strings.ToUpper(strings.TrimSpace(m.Code))
	room := h.codes[code]
	if room == nil || time.Now().After(room.codeExpires) {
		h.sendErr(c, protocol.ErrCodeBadCode, "unknown or expired code")
		return
	}
	if room == c.room {
		return
	}

	for _, t := range h.transfers {
		if t.FromID == c.ID || t.ToID == c.ID {
			h.failTransfer(t, "peer-left-room")
		}
	}
	h.leaveRoom(c)
	h.joinRoom(c, room)
	room.codeExpires = time.Now().Add(protocol.RoomCodeTTLSec * time.Second)
	c.log.Info("joined room by code", "room", room.key)
}

func (h *Hub) allowJoin(ip string, now time.Time) bool {
	b := h.buckets[ip]
	if b == nil {
		b = &bucket{tokens: joinBurst, last: now}
		h.buckets[ip] = b
	}
	b.tokens = min(joinBurst, b.tokens+now.Sub(b.last).Seconds()*joinRate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (h *Hub) handleOffer(c *Client, m protocol.TransferOffer) {
	id, err := uuid.Parse(m.ID)
	if err != nil || id == uuid.Nil {
		h.sendErr(c, protocol.ErrCodeBadMessage, "transfer id must be a UUID")
		return
	}
	if h.transfers[id] != nil {
		h.sendErr(c, protocol.ErrCodeBadTransfer, "transfer id already in use")
		return
	}
	name := sanitizeFilename(m.Name)
	if name == "" {
		h.sendErr(c, protocol.ErrCodeBadMessage, "bad filename")
		return
	}
	if m.Size <= 0 || m.Size > protocol.MaxDeclaredSize {
		h.sendErr(c, protocol.ErrCodeBadMessage, "size must be between 1 byte and 50GB")
		return
	}
	if len(m.Key) > protocol.MaxKeyChars {
		h.sendErr(c, protocol.ErrCodeBadMessage, "key too long")
		return
	}
	if !protocol.ValidPreview(m.Preview) {
		h.sendErr(c, protocol.ErrCodeBadMessage, "preview must be a data:image URL of at most 40KB")
		return
	}
	target := h.clients[m.To]
	if target == nil || target.room != c.room || target == c {
		h.sendErr(c, protocol.ErrCodeUnknownPeer, "no such peer in your room")
		return
	}
	live := 0
	for _, t := range h.transfers {
		if t.FromID == c.ID {
			live++
		}
	}
	if live >= maxTransfersPerSender {
		h.sendErr(c, protocol.ErrCodeBadTransfer, "too many concurrent transfers")
		return
	}

	mime := m.Mime
	if len(mime) > 128 {
		mime = ""
	}
	t := newTransfer(id, c.ID, target.ID, name, m.Size, mime, time.Now())
	t.Key = m.Key
	h.transfers[id] = t
	from := c.Peer()
	h.sendJSON(target, protocol.TypeTransferOffer, protocol.TransferOffer{
		ID: m.ID, From: &from, Name: name, Size: m.Size, Mime: mime, Key: m.Key, Preview: m.Preview,
	})
	h.logTransfer(t, "transfer offered")
}

func (h *Hub) handleAnswer(c *Client, m protocol.TransferAnswer) {
	t := h.lookupTransfer(c, m.ID)
	if t == nil {
		return
	}
	if len(m.Key) > protocol.MaxKeyChars {
		h.sendErr(c, protocol.ErrCodeBadMessage, "key too long")
		return
	}
	if err := t.Answer(c.ID, m.Accept); err != nil {
		h.sendErr(c, protocol.ErrCodeBadTransfer, err.Error())
		return
	}
	if t.Key == "" {
		m.Key = ""
	} else if m.Key != "" && m.Accept {
		t.Encrypt()
	}
	if sender := h.clients[t.FromID]; sender != nil {
		h.sendJSON(sender, protocol.TypeTransferAnswer, m)
	}
	if t.State == StateDeclined {
		delete(h.transfers, t.ID)
		h.logTransfer(t, "transfer declined")
	} else {
		h.logTransfer(t, "transfer accepted")
	}
}

func (h *Hub) handleCancel(c *Client, m protocol.TransferCancel) {
	t := h.lookupTransfer(c, m.ID)
	if t == nil {
		return
	}
	if err := t.Cancel(c.ID); err != nil {
		h.sendErr(c, protocol.ErrCodeBadTransfer, err.Error())
		return
	}
	other := t.ToID
	if c.ID == t.ToID {
		other = t.FromID
	}
	if o := h.clients[other]; o != nil {
		h.sendJSON(o, protocol.TypeTransferCancel, protocol.TransferCancel{ID: m.ID, Reason: m.Reason})
	}
	delete(h.transfers, t.ID)
	h.logTransfer(t, "transfer canceled", "by", c.ID)
}

func (h *Hub) handleProfile(c *Client, m protocol.Profile) {
	name, emoji := names.CleanName(m.Name), names.CleanEmoji(m.Emoji)
	if name == "" || emoji == "" {
		h.sendErr(c, protocol.ErrCodeBadMessage, "bad profile")
		return
	}
	c.Name, c.Emoji = name, emoji
	if c.room == nil {
		return
	}
	self := c.Peer()
	for _, other := range c.room.clients {
		h.sendJSON(other, protocol.TypePeerUpdated, self)
	}
}

func (h *Hub) handleSnippet(c *Client, m protocol.Snippet) {
	if m.Text == "" || len(m.Text) > protocol.MaxSnippetBytes {
		h.sendErr(c, protocol.ErrCodeBadMessage, "snippet must be 1..8192 bytes")
		return
	}
	target := h.clients[m.To]
	if target == nil || target.room != c.room || target == c {
		h.sendErr(c, protocol.ErrCodeUnknownPeer, "no such peer in your room")
		return
	}
	from := c.Peer()
	h.sendJSON(target, protocol.TypeSnippet, protocol.Snippet{From: &from, Text: m.Text})
}

func (h *Hub) handleRTC(c *Client, m protocol.RTC) {
	if len(m.Signal) == 0 || len(m.Signal) > protocol.MaxSignalBytes {
		h.sendErr(c, protocol.ErrCodeBadMessage, "signal must be 1..16384 bytes")
		return
	}
	target := h.clients[m.To]
	if target == nil || target.room != c.room || target == c {
		h.sendErr(c, protocol.ErrCodeUnknownPeer, "no such peer in your room")
		return
	}
	h.sendJSON(target, protocol.TypeRTC, protocol.RTC{From: c.ID, Signal: m.Signal})
}

func (h *Hub) lookupTransfer(c *Client, idStr string) *Transfer {
	id, err := uuid.Parse(idStr)
	if err != nil {
		h.sendErr(c, protocol.ErrCodeBadMessage, "transfer id must be a UUID")
		return nil
	}
	t := h.transfers[id]
	if t == nil {
		h.sendErr(c, protocol.ErrCodeBadTransfer, "unknown transfer")
		return nil
	}
	return t
}

func (h *Hub) handleFrame(m inbound) {
	c := m.from
	if _, ok := h.clients[c.ID]; !ok {
		framePool.Put(m.pool)
		return
	}
	id, chunk, err := protocol.SplitFrame(m.frame)
	if err != nil {
		framePool.Put(m.pool)
		h.sendErr(c, protocol.ErrCodeBadMessage, err.Error())
		return
	}
	t := h.transfers[id]
	if t == nil {

		framePool.Put(m.pool)
		return
	}
	wasAccepted := t.State == StateAccepted
	if err := t.Chunk(c.ID, len(chunk)); err != nil {
		framePool.Put(m.pool)
		h.sendErr(c, protocol.ErrCodeBadTransfer, err.Error())
		h.failTransfer(t, "protocol violation: "+err.Error())
		return
	}
	if wasAccepted {
		h.logTransfer(t, "transfer active")
	}
	receiver := h.clients[t.ToID]
	if receiver == nil {
		framePool.Put(m.pool)
		h.failTransfer(t, "peer-disconnected")
		return
	}
	if !h.trySend(receiver, outFrame{msgType: websocket.BinaryMessage, data: m.frame, pool: m.pool, transferID: id}) {

		framePool.Put(m.pool)
		h.failTransfer(t, "receiver-backpressure")
	}
}

func (h *Hub) handleWritten(w written) {
	t := h.transfers[w.id]
	if t == nil || w.to.ID != t.ToID {
		return
	}
	completed, err := t.NoteWritten(w.n)
	if err != nil {
		h.failTransfer(t, err.Error())
		return
	}
	if completed {
		done := protocol.TransferComplete{ID: t.ID.String(), Bytes: t.Written}
		if sender := h.clients[t.FromID]; sender != nil {
			h.sendJSON(sender, protocol.TypeTransferComplete, done)
		}
		h.sendJSON(w.to, protocol.TypeTransferComplete, done)
		delete(h.transfers, t.ID)
		h.logTransfer(t, "transfer completed")
		return
	}
	if sender := h.clients[t.FromID]; sender != nil {
		if !h.trySend(sender, outFrame{msgType: websocket.TextMessage,
			data: protocol.MustEncode(protocol.TypeFlowCredit, protocol.FlowCredit{ID: t.ID.String(), N: 1})}) {

			h.failTransfer(t, "sender-backpressure")
		}
	}
}

func (h *Hub) failTransfer(t *Transfer, reason string) {
	if !t.Fail() {
		return
	}
	msg := protocol.TransferFailed{ID: t.ID.String(), Reason: reason}
	if from := h.clients[t.FromID]; from != nil {
		h.sendJSON(from, protocol.TypeTransferFailed, msg)
	}
	if to := h.clients[t.ToID]; to != nil {
		h.sendJSON(to, protocol.TypeTransferFailed, msg)
	}
	delete(h.transfers, t.ID)
	h.logTransfer(t, "transfer failed", "reason", reason)
}

func (h *Hub) tick(now time.Time) {
	for _, t := range h.transfers {
		if t.State == StateOffered && now.After(t.Deadline) {
			h.failTransfer(t, "offer-timeout")
		}
	}
	for code, room := range h.codes {
		if now.After(room.codeExpires) {
			room.code = ""
			delete(h.codes, code)
		}
	}
	for ip, b := range h.buckets {
		if now.Sub(b.last) > 10*time.Minute {
			delete(h.buckets, ip)
		}
	}
}

func (h *Hub) logTransfer(t *Transfer, msg string, args ...any) {
	h.log.Info(msg, append([]any{
		"transfer", t.ID.String(), "from", t.FromID, "to", t.ToID,
		"name", t.Name, "size", t.Size, "e2e", t.Wire != t.Size, "state", t.State.String(),
	}, args...)...)
}

func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(name)
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	if name == "." || name == ".." {
		return ""
	}
	for len(name) > protocol.MaxFilenameBytes {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	return strings.TrimSpace(name)
}
