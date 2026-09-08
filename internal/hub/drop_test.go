package hub

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/TamerlanK/beam/internal/protocol"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

func dropHub() *Hub {
	return testHubLimits(Limits{MaxDropBytes: 1 << 20, DropBudget: 4 << 20, DropTTL: time.Minute})
}

func uploadDrop(t *testing.T, h *Hub, c *Client, id uuid.UUID, to string, size int64) [][]byte {
	t.Helper()
	sendText(h, c, protocol.TypeDropCreate, protocol.DropCreate{ID: id.String(), To: to, Name: "note.pdf", Size: size, Key: "PUB"})
	if lastOfType(drain(t, c), protocol.TypeDropCreated) == nil {
		t.Fatal("no drop-created")
	}
	var frames [][]byte
	wire := protocol.WireSize(size)
	for sent := int64(0); sent < wire; {
		n := min(int64(protocol.ChunkSize+protocol.TagBytes), wire-sent)
		buf := make([]byte, protocol.MaxFrameSize)
		chunk := bytes.Repeat([]byte{byte(len(frames) + 1)}, int(n))
		f := protocol.EncodeFrame(buf, id, chunk)
		frames = append(frames, append([]byte(nil), f...))
		h.handleFrame(inbound{from: c, frame: f, pool: &buf})
		sent += n
	}
	envs := drain(t, c)
	if lastOfType(envs, protocol.TypeDropStored) == nil {
		t.Fatalf("no drop-stored after full upload; got %+v", envs)
	}
	if lastOfType(envs, protocol.TypeFlowCredit) == nil && len(frames) > 1 {
		t.Fatal("upload got no flow credits")
	}
	return frames
}

func pickUp(t *testing.T, h *Hub, c *Client, id uuid.UUID, offset int64) [][]byte {
	t.Helper()
	sendText(h, c, protocol.TypeDropAccept, protocol.DropAccept{ID: id.String(), Offset: offset})
	var got [][]byte
	for {
		select {
		case f := <-c.send:
			if f.msgType == websocket.BinaryMessage {
				got = append(got, f.data)
				h.handleWritten(written{to: c, id: id, n: len(f.data) - protocol.FrameOverhead})
				continue
			}
			env, err := protocol.Decode(f.data)
			must(t, err)
			switch env.Type {
			case protocol.TypeTransferComplete:
				return got
			case protocol.TypeError, protocol.TypeTransferFailed:
				t.Fatalf("pickup failed: %s", env.Data)
			}
		default:
			t.Fatalf("pickup stalled after %d frames", len(got))
		}
	}
}

func TestDropUploadHoldAndPickup(t *testing.T) {
	h := dropHub()
	a := addTestClient(h, "a", "1.1.1.1")
	b := addTestClient(h, "b", "1.1.1.1")
	drain(t, a)
	drain(t, b)

	id := uuid.New()
	size := int64(2*protocol.ChunkSize + 123)
	frames := uploadDrop(t, h, a, id, "b", size)
	if len(frames) != 3 {
		t.Fatalf("uploaded %d frames, want 3", len(frames))
	}
	env := lastOfType(drain(t, b), protocol.TypeDropWaiting)
	if env == nil {
		t.Fatal("addressee got no drop-waiting once the upload completed")
	}
	var w protocol.DropWaiting
	must(t, json.Unmarshal(env.Data, &w))
	if w.From == nil || w.From.ID != "a" || w.Key != "PUB" || w.Size != size || w.Anyone || w.ExpiresIn < 50 {
		t.Fatalf("drop-waiting = %+v", w)
	}

	h.removeClient(a)
	if h.drops[id] == nil {
		t.Fatal("held drop was deleted when its creator disconnected")
	}

	h.removeClient(b)
	b = addTestClient(h, "b", "1.1.1.1")
	if lastOfType(drain(t, b), protocol.TypeDropWaiting) == nil {
		t.Fatal("reconnecting addressee was not told about its waiting drop")
	}

	got := pickUp(t, h, b, id, 0)
	if len(got) != len(frames) {
		t.Fatalf("picked up %d frames, want %d", len(got), len(frames))
	}
	for i := range frames {
		if !bytes.Equal(got[i], frames[i]) {
			t.Fatalf("frame %d differs", i)
		}
	}
	if h.drops[id] != nil || h.transfers[id] != nil || h.dropBytes != 0 {
		t.Fatalf("drop not released after pickup: drops=%d transfers=%d held=%d", len(h.drops), len(h.transfers), h.dropBytes)
	}
	if lastOfType(drain(t, b), protocol.TypeDropGone) == nil {
		t.Fatal("addressee got no drop-gone after pickup")
	}
}

func TestDropPickupResumesFromOffset(t *testing.T) {
	h := dropHub()
	a := addTestClient(h, "a", "1.1.1.1")
	b := addTestClient(h, "b", "1.1.1.1")
	drain(t, a)
	drain(t, b)

	id := uuid.New()
	frames := uploadDrop(t, h, a, id, "b", 4*protocol.ChunkSize)
	drain(t, b)

	sendText(h, b, protocol.TypeDropAccept, protocol.DropAccept{ID: id.String()})
	<-b.send
	h.removeClient(b)
	d := h.drops[id]
	if d == nil || d.pickup != nil {
		t.Fatalf("drop after receiver vanished mid-pickup: %+v", d)
	}
	if h.transfers[id] != nil {
		t.Fatal("dangling pickup transfer")
	}

	b = addTestClient(h, "b", "1.1.1.1")
	drain(t, b)
	got := pickUp(t, h, b, id, 2*protocol.ChunkSize)
	if len(got) != 2 || !bytes.Equal(got[0], frames[2]) || !bytes.Equal(got[1], frames[3]) {
		t.Fatalf("resume from chunk 2 delivered %d frames", len(got))
	}
}

func TestDropLinkClaimAndAnyoneAccept(t *testing.T) {
	h := dropHub()
	a := addTestClient(h, "a", "1.1.1.1")
	stranger := addTestClient(h, "s", "9.9.9.9")
	drain(t, a)
	drain(t, stranger)

	id := uuid.New()
	uploadDrop(t, h, a, id, "", 10)
	h.removeClient(a)

	sendText(h, stranger, protocol.TypeDropClaim, protocol.DropClaim{ID: id.String()})
	env := lastOfType(drain(t, stranger), protocol.TypeDropWaiting)
	if env == nil {
		t.Fatal("link drop could not be claimed by id")
	}
	var w protocol.DropWaiting
	must(t, json.Unmarshal(env.Data, &w))
	if !w.Anyone || w.Key != "PUB" {
		t.Fatalf("claimed drop-waiting = %+v", w)
	}
	if got := pickUp(t, h, stranger, id, 0); len(got) != 1 {
		t.Fatalf("picked up %d frames", len(got))
	}
	sendText(h, stranger, protocol.TypeDropClaim, protocol.DropClaim{ID: id.String()})
	if e := lastOfType(drain(t, stranger), protocol.TypeError); e == nil {
		t.Fatal("second claim of a picked-up drop did not error")
	}
}

func TestDropAddresseeOnly(t *testing.T) {
	h := dropHub()
	a := addTestClient(h, "a", "1.1.1.1")
	b := addTestClient(h, "b", "1.1.1.1")
	s := addTestClient(h, "s", "1.1.1.1")
	drain(t, a)
	drain(t, b)
	drain(t, s)

	id := uuid.New()
	uploadDrop(t, h, a, id, "b", 10)
	if lastOfType(drain(t, s), protocol.TypeDropWaiting) != nil {
		t.Fatal("a bystander was told about someone else's drop")
	}
	for _, msgType := range []string{protocol.TypeDropClaim, protocol.TypeDropAccept} {
		sendText(h, s, msgType, protocol.DropClaim{ID: id.String()})
		env := lastOfType(drain(t, s), protocol.TypeError)
		if env == nil {
			t.Fatalf("%s by a non-addressee succeeded", msgType)
		}
		var e protocol.Error
		must(t, json.Unmarshal(env.Data, &e))
		if e.Code != protocol.ErrCodeBadDrop {
			t.Fatalf("%s error code = %s", msgType, e.Code)
		}
	}

	sendText(h, s, protocol.TypeDropCancel, protocol.DropCancel{ID: id.String()})
	if h.drops[id] == nil {
		t.Fatal("bystander canceled someone else's drop")
	}

	sendText(h, b, protocol.TypeDropCancel, protocol.DropCancel{ID: id.String()})
	if h.drops[id] != nil {
		t.Fatal("addressee could not decline the drop")
	}
	if lastOfType(drain(t, a), protocol.TypeDropGone) == nil {
		t.Fatal("creator was not told the drop was declined")
	}
}

func TestDropLimitsAndValidation(t *testing.T) {
	h := testHubLimits(Limits{MaxDropBytes: 1000, DropBudget: 2500})
	a := addTestClient(h, "a", "1.1.1.1")
	b := addTestClient(h, "b", "1.1.1.1")
	s := addTestClient(h, "s", "7.7.7.7")
	drain(t, a)
	drain(t, b)
	drain(t, s)

	create := func(c *Client, m protocol.DropCreate) *protocol.Envelope {
		t.Helper()
		if m.ID == "" {
			m.ID = uuid.NewString()
		}
		if m.Name == "" {
			m.Name = "x"
		}
		sendText(h, c, protocol.TypeDropCreate, m)
		return lastOfType(drain(t, c), protocol.TypeError)
	}
	bad := map[string]protocol.DropCreate{
		"oversize":      {Size: 1001, To: "b"},
		"zero":          {Size: 0, To: "b"},
		"cross-room to": {Size: 1, To: "s"},
		"self":          {Size: 1, To: "a"},
		"bad name":      {Size: 1, To: "b", Name: ".."},
		"bad id":        {Size: 1, To: "b", ID: "nope"},
	}
	for name, m := range bad {
		if create(a, m) == nil {
			t.Fatalf("%s: invalid drop-create produced no error", name)
		}
	}
	if create(a, protocol.DropCreate{Size: 1000, To: "b"}) != nil {
		t.Fatal("valid drop rejected")
	}
	if create(a, protocol.DropCreate{Size: 1000}) != nil {
		t.Fatal("valid link drop rejected")
	}
	if e := create(a, protocol.DropCreate{Size: 1000, To: "b"}); e == nil {
		t.Fatal("drop over the global budget was accepted")
	}
	if h.dropBytes != 2*protocol.WireSize(1000) {
		t.Fatalf("held bytes = %d", h.dropBytes)
	}

	off := testHub()
	c := addTestClient(off, "c", "1.1.1.1")
	drain(t, c)
	sendText(off, c, protocol.TypeDropCreate, protocol.DropCreate{ID: uuid.NewString(), Name: "x", Size: 1})
	if lastOfType(drain(t, c), protocol.TypeError) == nil {
		t.Fatal("drops accepted while disabled")
	}
	env := lastOfType(drain(t, addTestClient(off, "d", "1.1.1.1")), protocol.TypeRoomState)
	var rs protocol.RoomState
	must(t, json.Unmarshal(env.Data, &rs))
	if rs.Drops != nil {
		t.Fatal("room-state advertises drops while disabled")
	}
}

func TestDropExpiryAndPartialUpload(t *testing.T) {
	h := dropHub()
	a := addTestClient(h, "a", "1.1.1.1")
	b := addTestClient(h, "b", "1.1.1.1")
	drain(t, a)
	drain(t, b)

	id := uuid.New()
	uploadDrop(t, h, a, id, "b", 10)
	drain(t, b)
	h.drops[id].Expires = time.Now().Add(-time.Second)
	h.tick(time.Now())
	if h.drops[id] != nil || h.dropBytes != 0 {
		t.Fatal("expired drop not reaped")
	}
	var gone protocol.DropGone
	env := lastOfType(drain(t, b), protocol.TypeDropGone)
	if env == nil {
		t.Fatal("addressee not told the drop expired")
	}
	must(t, json.Unmarshal(env.Data, &gone))
	if gone.Reason != "expired" {
		t.Fatalf("reason = %s", gone.Reason)
	}

	id2 := uuid.New()
	sendText(h, a, protocol.TypeDropCreate, protocol.DropCreate{ID: id2.String(), To: "b", Name: "big", Size: 3 * protocol.ChunkSize})
	drain(t, a)
	buf := make([]byte, protocol.MaxFrameSize)
	h.handleFrame(inbound{from: a, frame: protocol.EncodeFrame(buf, id2, make([]byte, protocol.ChunkSize+protocol.TagBytes)), pool: &buf})
	h.removeClient(a)
	if h.drops[id2] != nil || h.dropBytes != 0 {
		t.Fatal("partial upload survived its creator")
	}
	if lastOfType(drain(t, b), protocol.TypeDropWaiting) != nil {
		t.Fatal("addressee was offered a partial drop")
	}

	c := addTestClient(h, "c", "1.1.1.1")
	drain(t, c)
	id3 := uuid.New()
	sendText(h, c, protocol.TypeDropCreate, protocol.DropCreate{ID: id3.String(), To: "b", Name: "small", Size: 10})
	drain(t, c)
	buf = make([]byte, protocol.MaxFrameSize)
	h.handleFrame(inbound{from: c, frame: protocol.EncodeFrame(buf, id3, make([]byte, 100)), pool: &buf})
	if h.drops[id3] != nil {
		t.Fatal("oversized upload was kept")
	}
	if lastOfType(drain(t, c), protocol.TypeError) == nil {
		t.Fatal("oversized upload produced no error")
	}
}

func TestDropUploadHonorsRelayBudget(t *testing.T) {
	h := testHubLimits(Limits{MaxDropBytes: 1 << 20, RelayBytesPerSec: protocol.ChunkSize + protocol.TagBytes})
	a := addTestClient(h, "a", "1.1.1.1")
	drain(t, a)
	id := uuid.New()
	sendText(h, a, protocol.TypeDropCreate, protocol.DropCreate{ID: id.String(), Name: "x", Size: 4 * protocol.ChunkSize})
	drain(t, a)
	h.refillRelay()
	for range 2 {
		buf := make([]byte, protocol.MaxFrameSize)
		h.handleFrame(inbound{from: a, frame: protocol.EncodeFrame(buf, id, make([]byte, protocol.ChunkSize+protocol.TagBytes)), pool: &buf})
	}
	if n := len(lastOfTypeAll(drain(t, a), protocol.TypeFlowCredit)); n != 1 {
		t.Fatalf("got %d credits under a one-chunk budget, want 1", n)
	}
	h.refillRelay()
	if n := len(lastOfTypeAll(drain(t, a), protocol.TypeFlowCredit)); n != 1 {
		t.Fatalf("refill released %d starved credits, want 1", n)
	}
	if len(h.starved) != 0 {
		t.Fatalf("starved queue not drained: %d", len(h.starved))
	}
}

func lastOfTypeAll(envs []protocol.Envelope, msgType string) []protocol.Envelope {
	var out []protocol.Envelope
	for _, e := range envs {
		if e.Type == msgType {
			out = append(out, e)
		}
	}
	return out
}
