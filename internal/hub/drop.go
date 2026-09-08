package hub

import (
	"fmt"
	"time"

	"github.com/TamerlanK/beam/internal/protocol"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const maxDropsPerIP = 16

type Drop struct {
	ID      uuid.UUID
	FromID  string
	From    protocol.Peer
	ToID    string
	IP      string
	Name    string
	Path    string
	Size    int64
	Wire    int64
	Mime    string
	Key     string
	Preview string

	frames  [][]byte
	got     int64
	Expires time.Time
	pickup  *Transfer
}

func (d *Drop) Held() bool { return d.got == d.Wire }

func (h *Hub) dropsEnabled() bool { return h.limits.MaxDropBytes > 0 }

func (h *Hub) dropLimits() *protocol.DropLimits {
	if !h.dropsEnabled() {
		return nil
	}
	return &protocol.DropLimits{MaxBytes: h.limits.MaxDropBytes, TTLSec: int(h.limits.DropTTL.Seconds())}
}

func (h *Hub) handleDropCreate(c *Client, m protocol.DropCreate) {
	if !h.dropsEnabled() {
		h.sendErr(c, protocol.ErrCodeBadDrop, "Drops are disabled on this server")
		return
	}
	id, err := uuid.Parse(m.ID)
	if err != nil || id == uuid.Nil {
		h.sendErr(c, protocol.ErrCodeBadMessage, "drop id must be a UUID")
		return
	}
	if h.transfers[id] != nil || h.drops[id] != nil {
		h.sendErr(c, protocol.ErrCodeBadTransfer, "drop id already in use")
		return
	}
	name := sanitizeFilename(m.Name)
	if name == "" {
		h.sendErr(c, protocol.ErrCodeBadMessage, "bad filename")
		return
	}
	dir, ok := sanitizePath(m.Path)
	if !ok {
		h.sendErr(c, protocol.ErrCodeBadMessage, "path must be relative folder segments of at most 1KB")
		return
	}
	if m.Size <= 0 || m.Size > h.limits.MaxDropBytes {
		h.sendErr(c, protocol.ErrCodeBadDrop, "Drops can be at most "+humanBytes(h.limits.MaxDropBytes))
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
	if m.To != "" {
		target := h.clients[m.To]
		if target == nil || target.room != c.room || target == c {
			h.sendErr(c, protocol.ErrCodeUnknownPeer, "no such peer in your room")
			return
		}
	}
	mine := 0
	for _, d := range h.drops {
		if d.IP == c.IP {
			mine++
		}
	}
	if mine >= maxDropsPerIP {
		h.sendErr(c, protocol.ErrCodeBadDrop, "Too many drops from your address")
		return
	}
	wire := protocol.WireSize(m.Size)
	if h.limits.DropBudget > 0 && h.dropBytes+wire > h.limits.DropBudget {
		h.sendErr(c, protocol.ErrCodeBadDrop, "The drop box is full right now, try again later")
		return
	}
	mime := m.Mime
	if len(mime) > 128 {
		mime = ""
	}
	now := time.Now()
	d := &Drop{
		ID: id, FromID: c.ID, From: c.Peer(), ToID: m.To, IP: c.IP,
		Name: name, Path: dir, Size: m.Size, Wire: wire, Mime: mime, Key: m.Key, Preview: m.Preview,
		frames:  make([][]byte, 0, (m.Size+protocol.ChunkSize-1)/protocol.ChunkSize),
		Expires: now.Add(h.limits.DropTTL),
	}
	h.drops[id] = d
	h.dropBytes += wire
	h.sendJSON(c, protocol.TypeDropCreated, protocol.DropCreated{ID: m.ID, ExpiresIn: int(h.limits.DropTTL.Seconds())})
	h.logDrop(d, "drop created")
}

func (h *Hub) storeFrame(c *Client, d *Drop, m inbound) {
	defer framePool.Put(m.pool)
	if d.FromID != c.ID || d.Held() {
		return
	}
	n := int64(len(m.frame) - protocol.FrameOverhead)
	if d.got+n > d.Wire {
		h.sendErr(c, protocol.ErrCodeBadDrop, fmt.Sprintf("received %d bytes, more than declared size %d", d.got+n, d.Wire))
		h.deleteDrop(d, "protocol violation")
		return
	}
	d.frames = append(d.frames, append([]byte(nil), m.frame...))
	d.got += n
	if d.Held() {
		d.Expires = time.Now().Add(h.limits.DropTTL)
		h.sendJSON(c, protocol.TypeDropStored, protocol.DropStored{ID: d.ID.String(), ExpiresIn: int(h.limits.DropTTL.Seconds())})
		h.logDrop(d, "drop stored")
		h.announce(d)
		return
	}
	credit := func() {
		if h.drops[d.ID] != d || d.Held() {
			return
		}
		if from := h.clients[d.FromID]; from != nil {
			h.sendJSON(from, protocol.TypeFlowCredit, protocol.FlowCredit{ID: d.ID.String(), N: 1})
		}
	}
	if h.limits.RelayBytesPerSec > 0 {
		h.relayTokens -= n
		if h.relayTokens < 0 {
			h.starved = append(h.starved, credit)
			return
		}
	}
	credit()
}

func (h *Hub) waiting(d *Drop) protocol.DropWaiting {
	from := d.From
	return protocol.DropWaiting{
		ID: d.ID.String(), From: &from, Name: d.Name, Path: d.Path, Size: d.Size, Mime: d.Mime, Key: d.Key, Preview: d.Preview,
		ExpiresIn: int(time.Until(d.Expires).Seconds()), Anyone: d.ToID == "",
	}
}

func (h *Hub) announce(d *Drop) {
	if d.ToID == "" || d.pickup != nil || !d.Held() {
		return
	}
	if to := h.clients[d.ToID]; to != nil {
		h.sendJSON(to, protocol.TypeDropWaiting, h.waiting(d))
	}
}

func (h *Hub) announceTo(c *Client) {
	for _, d := range h.drops {
		if d.ToID == c.ID {
			h.announce(d)
		}
	}
}

func (h *Hub) lookupDrop(c *Client, idStr string) *Drop {
	id, err := uuid.Parse(idStr)
	if err != nil {
		h.sendErr(c, protocol.ErrCodeBadMessage, "drop id must be a UUID")
		return nil
	}
	d := h.drops[id]
	if d == nil || !d.Held() {
		h.sendErr(c, protocol.ErrCodeBadDrop, "That drop is gone or was already picked up")
		return nil
	}
	return d
}

func (h *Hub) handleDropClaim(c *Client, m protocol.DropClaim) {
	if !h.allowJoin(c.IP, time.Now()) {
		h.sendErr(c, protocol.ErrCodeRateLimited, "too many attempts, slow down")
		return
	}
	d := h.lookupDrop(c, m.ID)
	if d == nil {
		return
	}
	if d.ToID != "" && d.ToID != c.ID {
		h.sendErr(c, protocol.ErrCodeBadDrop, "That drop is addressed to another device")
		return
	}
	if d.pickup != nil {
		h.sendErr(c, protocol.ErrCodeBadDrop, "That drop is being picked up right now")
		return
	}
	h.sendJSON(c, protocol.TypeDropWaiting, h.waiting(d))
}

func (h *Hub) handleDropAccept(c *Client, m protocol.DropAccept) {
	d := h.lookupDrop(c, m.ID)
	if d == nil {
		return
	}
	if d.ToID != "" && d.ToID != c.ID {
		h.sendErr(c, protocol.ErrCodeBadDrop, "That drop is addressed to another device")
		return
	}
	if d.pickup != nil {
		h.sendErr(c, protocol.ErrCodeBadDrop, "That drop is being picked up right now")
		return
	}
	if m.Offset != 0 && !protocol.ValidOffset(m.Offset, d.Size) {
		h.sendErr(c, protocol.ErrCodeBadMessage, "offset must be a chunk-aligned position below size")
		return
	}
	t := newTransfer(d.ID, d.FromID, c.ID, d.Name, d.Size, d.Mime, time.Now())
	t.Key = d.Key
	t.Encrypt()
	t.State = StateActive
	t.Start(m.Offset)
	t.drop = d
	t.cursor = int(m.Offset / protocol.ChunkSize)
	d.pickup = t
	h.transfers[t.ID] = t
	h.logTransfer(t, "drop pickup started")
	for range protocol.CreditWindow {
		if !h.pushDrop(t) {
			return
		}
	}
}

func (h *Hub) pushDrop(t *Transfer) bool {
	d := t.drop
	if h.transfers[t.ID] != t || t.cursor >= len(d.frames) {
		return false
	}
	f := d.frames[t.cursor]
	if err := t.Chunk(d.FromID, len(f)-protocol.FrameOverhead); err != nil {
		h.failTransfer(t, "protocol violation: "+err.Error())
		return false
	}
	receiver := h.clients[t.ToID]
	if receiver == nil {
		h.failTransfer(t, "peer-disconnected")
		return false
	}
	if !h.trySend(receiver, outFrame{msgType: websocket.BinaryMessage, data: f, transferID: t.ID}) {
		h.failTransfer(t, "receiver-backpressure")
		return false
	}
	t.cursor++
	return true
}

func (h *Hub) handleDropCancel(c *Client, m protocol.DropCancel) {
	id, err := uuid.Parse(m.ID)
	if err != nil {
		h.sendErr(c, protocol.ErrCodeBadMessage, "drop id must be a UUID")
		return
	}
	d := h.drops[id]
	if d == nil {
		return
	}
	if c.ID != d.FromID && d.ToID != "" && c.ID != d.ToID {
		h.sendErr(c, protocol.ErrCodeBadDrop, "Not a party to that drop")
		return
	}
	h.deleteDrop(d, "canceled")
	h.logDrop(d, "drop canceled", "by", c.ID)
}

func (h *Hub) deleteDrop(d *Drop, reason string) {
	if h.drops[d.ID] != d {
		return
	}
	delete(h.drops, d.ID)
	h.dropBytes -= d.Wire
	d.frames = nil
	if p := d.pickup; p != nil {
		d.pickup = nil
		h.failTransfer(p, reason)
	}
	gone := protocol.DropGone{ID: d.ID.String(), Reason: reason}
	for _, id := range []string{d.FromID, d.ToID} {
		if c := h.clients[id]; c != nil {
			h.sendJSON(c, protocol.TypeDropGone, gone)
		}
	}
}

func (h *Hub) reapDrops(now time.Time) {
	for _, d := range h.drops {
		if d.pickup == nil && now.After(d.Expires) {
			h.deleteDrop(d, "expired")
			h.logDrop(d, "drop expired")
		}
	}
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%d GB", n>>30)
	case n >= 1<<20:
		return fmt.Sprintf("%d MB", n>>20)
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}

func (h *Hub) logDrop(d *Drop, msg string, args ...any) {
	h.log.Info(msg, append([]any{
		"drop", d.ID.String(), "from", d.FromID, "to", d.ToID, "name", d.Name, "size", d.Size, "held", h.dropBytes,
	}, args...)...)
}
