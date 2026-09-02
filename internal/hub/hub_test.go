package hub

import (
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/TamerlanK/beam/internal/protocol"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

func testHub() *Hub {
	return New(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func addTestClient(h *Hub, id, ip string) *Client {
	c := &Client{ID: id, Name: "Test " + id, Emoji: "🧪", Device: "laptop", IP: ip,
		hub: h, send: make(chan outFrame, sendBufSize), log: h.log}
	h.addClient(c)
	return c
}

func drain(t *testing.T, c *Client) []protocol.Envelope {
	t.Helper()
	var out []protocol.Envelope
	for {
		select {
		case f := <-c.send:
			if f.msgType != websocket.TextMessage {
				out = append(out, protocol.Envelope{Type: "binary"})
				continue
			}
			env, err := protocol.Decode(f.data)
			if err != nil {
				t.Fatalf("hub sent invalid envelope: %v", err)
			}
			out = append(out, env)
		default:
			return out
		}
	}
}

func lastOfType(envs []protocol.Envelope, msgType string) *protocol.Envelope {
	for i := len(envs) - 1; i >= 0; i-- {
		if envs[i].Type == msgType {
			return &envs[i]
		}
	}
	return nil
}

func sendText(h *Hub, c *Client, msgType string, data any) {
	h.handleText(c, protocol.MustEncode(msgType, data))
}

func TestRoomKeyedByIP(t *testing.T) {
	h := testHub()
	a := addTestClient(h, "a", "1.2.3.4")
	b := addTestClient(h, "b", "1.2.3.4")
	c := addTestClient(h, "c", "9.9.9.9")

	if a.room != b.room {
		t.Fatal("same-IP clients not grouped into one room")
	}
	if c.room == a.room {
		t.Fatal("different-IP client landed in the same room")
	}

	if lastOfType(drain(t, a), protocol.TypePeerJoined) == nil {
		t.Fatal("a never saw peer-joined for b")
	}

	env := lastOfType(drain(t, b), protocol.TypeRoomState)
	if env == nil {
		t.Fatal("b got no room-state")
	}
	var rs protocol.RoomState
	must(t, json.Unmarshal(env.Data, &rs))
	if len(rs.Peers) != 1 || rs.Peers[0].ID != "a" {
		t.Fatalf("b's room-state peers = %+v", rs.Peers)
	}
}

func TestRoomCodeJoinAndTTL(t *testing.T) {
	h := testHub()
	a := addTestClient(h, "a", "1.1.1.1")
	b := addTestClient(h, "b", "2.2.2.2")
	drain(t, a)
	drain(t, b)

	sendText(h, a, protocol.TypeRoomCreate, nil)
	env := lastOfType(drain(t, a), protocol.TypeRoomCreated)
	if env == nil {
		t.Fatal("no room-created reply")
	}
	var rc protocol.RoomCreated
	must(t, json.Unmarshal(env.Data, &rc))
	if len(rc.Code) != protocol.RoomCodeLen || rc.ExpiresIn != protocol.RoomCodeTTLSec {
		t.Fatalf("room-created = %+v", rc)
	}

	sendText(h, b, protocol.TypeRoomJoin, protocol.RoomJoin{Code: rc.Code})
	if a.room != b.room {
		t.Fatal("join by code did not move b into a's room")
	}
	if lastOfType(drain(t, b), protocol.TypeRoomState) == nil {
		t.Fatal("b got no room-state after joining")
	}

	a.room.codeExpires = time.Now().Add(-time.Minute)
	h.tick(time.Now())
	if h.codes[rc.Code] != nil {
		t.Fatal("tick did not reap the expired code")
	}
	c := addTestClient(h, "c", "3.3.3.3")
	drain(t, c)
	sendText(h, c, protocol.TypeRoomJoin, protocol.RoomJoin{Code: rc.Code})
	env = lastOfType(drain(t, c), protocol.TypeError)
	if env == nil {
		t.Fatal("joining an expired code did not error")
	}
	var e protocol.Error
	must(t, json.Unmarshal(env.Data, &e))
	if e.Code != protocol.ErrCodeBadCode {
		t.Fatalf("error code = %s, want %s", e.Code, protocol.ErrCodeBadCode)
	}
}

func TestJoinRateLimit(t *testing.T) {
	h := testHub()
	c := addTestClient(h, "c", "5.5.5.5")
	drain(t, c)
	limited := false
	for range joinBurst + 2 {
		sendText(h, c, protocol.TypeRoomJoin, protocol.RoomJoin{Code: "ZZZZ"})
		env := lastOfType(drain(t, c), protocol.TypeError)
		if env == nil {
			t.Fatal("bogus join produced no error")
		}
		var e protocol.Error
		must(t, json.Unmarshal(env.Data, &e))
		if e.Code == protocol.ErrCodeRateLimited {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatalf("no rate limiting after %d rapid join attempts", joinBurst+2)
	}
}

func TestOfferAnswerRelayFlow(t *testing.T) {
	h := testHub()
	a := addTestClient(h, "a", "1.1.1.1")
	b := addTestClient(h, "b", "1.1.1.1")
	drain(t, a)
	drain(t, b)

	id := uuid.New()
	sendText(h, a, protocol.TypeTransferOffer, protocol.TransferOffer{
		ID: id.String(), To: "b", Name: "x.bin", Size: 3 * protocol.ChunkSize,
	})
	env := lastOfType(drain(t, b), protocol.TypeTransferOffer)
	if env == nil {
		t.Fatal("b got no forwarded offer")
	}
	var off protocol.TransferOffer
	must(t, json.Unmarshal(env.Data, &off))
	if off.From == nil || off.From.ID != "a" || off.To != "" {
		t.Fatalf("forwarded offer = %+v; want From=a, To scrubbed", off)
	}

	sendText(h, b, protocol.TypeTransferAnswer, protocol.TransferAnswer{ID: id.String(), Accept: true})
	if lastOfType(drain(t, a), protocol.TypeTransferAnswer) == nil {
		t.Fatal("a never saw the acceptance")
	}

	frame := make([]byte, protocol.MaxFrameSize)
	h.handleFrame(inbound{from: a, frame: protocol.EncodeFrame(frame, id, make([]byte, protocol.ChunkSize)), pool: &frame})
	tr := h.transfers[id]
	if tr == nil || tr.State != StateActive {
		t.Fatalf("transfer state after first chunk = %v", tr)
	}
	select {
	case f := <-b.send:
		if f.msgType != websocket.BinaryMessage || len(f.data) != protocol.FrameOverhead+protocol.ChunkSize {
			t.Fatalf("b received msgType=%d len=%d", f.msgType, len(f.data))
		}

		h.handleWritten(written{to: b, id: id, n: protocol.ChunkSize})
	default:
		t.Fatal("chunk was not relayed to b")
	}
	if lastOfType(drain(t, a), protocol.TypeFlowCredit) == nil {
		t.Fatal("sender got no flow credit after receiver write")
	}

	for range 2 {
		buf := make([]byte, protocol.MaxFrameSize)
		h.handleFrame(inbound{from: a, frame: protocol.EncodeFrame(buf, id, make([]byte, protocol.ChunkSize)), pool: &buf})
		<-b.send
		h.handleWritten(written{to: b, id: id, n: protocol.ChunkSize})
	}
	if lastOfType(drain(t, a), protocol.TypeTransferComplete) == nil {
		t.Fatal("sender got no transfer-complete")
	}
	if lastOfType(drain(t, b), protocol.TypeTransferComplete) == nil {
		t.Fatal("receiver got no transfer-complete")
	}
	if h.transfers[id] != nil {
		t.Fatal("completed transfer not forgotten")
	}
}

func TestOfferValidation(t *testing.T) {
	h := testHub()
	a := addTestClient(h, "a", "1.1.1.1")
	b := addTestClient(h, "b", "1.1.1.1")
	stranger := addTestClient(h, "s", "7.7.7.7")
	drain(t, a)
	drain(t, b)
	drain(t, stranger)

	cases := []struct {
		name  string
		offer protocol.TransferOffer
	}{
		{"bad uuid", protocol.TransferOffer{ID: "nope", To: "b", Name: "x", Size: 1}},
		{"zero size", protocol.TransferOffer{ID: uuid.NewString(), To: "b", Name: "x", Size: 0}},
		{"oversize", protocol.TransferOffer{ID: uuid.NewString(), To: "b", Name: "x", Size: protocol.MaxDeclaredSize + 1}},
		{"self target", protocol.TransferOffer{ID: uuid.NewString(), To: "a", Name: "x", Size: 1}},
		{"cross-room target", protocol.TransferOffer{ID: uuid.NewString(), To: "s", Name: "x", Size: 1}},
		{"unknown target", protocol.TransferOffer{ID: uuid.NewString(), To: "ghost", Name: "x", Size: 1}},
		{"empty filename", protocol.TransferOffer{ID: uuid.NewString(), To: "b", Name: "..", Size: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sendText(h, a, protocol.TypeTransferOffer, tc.offer)
			if lastOfType(drain(t, a), protocol.TypeError) == nil {
				t.Fatal("invalid offer produced no error")
			}
			if got := drain(t, b); len(got) != 0 {
				t.Fatalf("invalid offer leaked to target: %+v", got)
			}
			if len(h.transfers) != 0 {
				t.Fatal("invalid offer created a transfer")
			}
		})
	}
}

func TestOfferTimeout(t *testing.T) {
	h := testHub()
	a := addTestClient(h, "a", "1.1.1.1")
	b := addTestClient(h, "b", "1.1.1.1")
	drain(t, a)
	drain(t, b)

	id := uuid.NewString()
	sendText(h, a, protocol.TypeTransferOffer, protocol.TransferOffer{ID: id, To: "b", Name: "x", Size: 1})
	h.tick(time.Now().Add((protocol.OfferTimeoutSec + 1) * time.Second))
	for _, c := range []*Client{a, b} {
		env := lastOfType(drain(t, c), protocol.TypeTransferFailed)
		if env == nil {
			t.Fatalf("client %s got no transfer-failed after timeout", c.ID)
		}
		var f protocol.TransferFailed
		must(t, json.Unmarshal(env.Data, &f))
		if f.Reason != "offer-timeout" {
			t.Fatalf("reason = %q", f.Reason)
		}
	}
	if len(h.transfers) != 0 {
		t.Fatal("timed-out transfer not forgotten")
	}
}

func TestDisconnectFailsOwnTransfersOnly(t *testing.T) {
	h := testHub()
	a := addTestClient(h, "a", "1.1.1.1")
	b := addTestClient(h, "b", "1.1.1.1")
	c := addTestClient(h, "c", "1.1.1.1")
	d := addTestClient(h, "d", "1.1.1.1")
	for _, cl := range []*Client{a, b, c, d} {
		drain(t, cl)
	}

	id1, id2 := uuid.NewString(), uuid.NewString()
	sendText(h, a, protocol.TypeTransferOffer, protocol.TransferOffer{ID: id1, To: "b", Name: "x", Size: 10})
	sendText(h, c, protocol.TypeTransferOffer, protocol.TransferOffer{ID: id2, To: "d", Name: "y", Size: 10})
	sendText(h, b, protocol.TypeTransferAnswer, protocol.TransferAnswer{ID: id1, Accept: true})
	sendText(h, d, protocol.TypeTransferAnswer, protocol.TransferAnswer{ID: id2, Accept: true})

	h.removeClient(a)
	env := lastOfType(drain(t, b), protocol.TypeTransferFailed)
	if env == nil {
		t.Fatal("b's transfer survived its peer disconnecting")
	}
	if lastOfType(drain(t, d), protocol.TypeTransferFailed) != nil {
		t.Fatal("unrelated transfer was failed by a's disconnect")
	}
	if len(h.transfers) != 1 {
		t.Fatalf("transfers left = %d, want 1", len(h.transfers))
	}

	if lastOfType(drain(t, c), protocol.TypePeerLeft) == nil {
		t.Fatal("no peer-left broadcast")
	}
}

func TestSnippetValidationAndRelay(t *testing.T) {
	h := testHub()
	a := addTestClient(h, "a", "1.1.1.1")
	b := addTestClient(h, "b", "1.1.1.1")
	drain(t, a)
	drain(t, b)

	sendText(h, a, protocol.TypeSnippet, protocol.Snippet{To: "b", Text: "https://example.com"})
	env := lastOfType(drain(t, b), protocol.TypeSnippet)
	if env == nil {
		t.Fatal("snippet not relayed")
	}
	var s protocol.Snippet
	must(t, json.Unmarshal(env.Data, &s))
	if s.From == nil || s.From.ID != "a" || s.Text != "https://example.com" {
		t.Fatalf("relayed snippet = %+v", s)
	}

	big := make([]byte, protocol.MaxSnippetBytes+1)
	for i := range big {
		big[i] = 'a'
	}
	sendText(h, a, protocol.TypeSnippet, protocol.Snippet{To: "b", Text: string(big)})
	if lastOfType(drain(t, a), protocol.TypeError) == nil {
		t.Fatal("oversize snippet accepted")
	}
}

func TestProfileUpdateAndIDCollision(t *testing.T) {
	h := testHub()
	a := addTestClient(h, "a", "1.1.1.1")
	b := addTestClient(h, "b", "1.1.1.1")
	drain(t, a)
	drain(t, b)

	sendText(h, a, protocol.TypeProfile, protocol.Profile{Name: "  Bob   the Builder ", Emoji: "🦊"})
	for _, c := range []*Client{a, b} {
		env := lastOfType(drain(t, c), protocol.TypePeerUpdated)
		if env == nil {
			t.Fatalf("client %s got no peer-updated", c.ID)
		}
		var p protocol.Peer
		must(t, json.Unmarshal(env.Data, &p))
		if p.ID != "a" || p.Name != "Bob the Builder" || p.Emoji != "🦊" {
			t.Fatalf("peer-updated = %+v", p)
		}
	}

	sendText(h, a, protocol.TypeProfile, protocol.Profile{Name: "x", Emoji: "ab"})
	if lastOfType(drain(t, a), protocol.TypeError) == nil {
		t.Fatal("text avatar accepted")
	}
	if a.Emoji != "🦊" {
		t.Fatalf("rejected profile still applied: %q", a.Emoji)
	}

	dup := addTestClient(h, "a", "1.1.1.1")
	if dup.ID == "a" || h.clients["a"] != a {
		t.Fatalf("duplicate id not reassigned: %q", dup.ID)
	}
}

func TestRTCSignalForwarded(t *testing.T) {
	h := testHub()
	a := addTestClient(h, "a", "1.1.1.1")
	b := addTestClient(h, "b", "1.1.1.1")
	c := addTestClient(h, "c", "2.2.2.2")
	drain(t, a)
	drain(t, b)
	drain(t, c)

	sendText(h, a, protocol.TypeRTC, protocol.RTC{To: "b", Signal: json.RawMessage(`{"candidate":"x"}`)})
	env := lastOfType(drain(t, b), protocol.TypeRTC)
	if env == nil {
		t.Fatal("b got no rtc signal")
	}
	var m protocol.RTC
	must(t, json.Unmarshal(env.Data, &m))
	if m.From != "a" || m.To != "" || string(m.Signal) != `{"candidate":"x"}` {
		t.Fatalf("forwarded rtc = %+v", m)
	}

	sendText(h, a, protocol.TypeRTC, protocol.RTC{To: "c", Signal: json.RawMessage(`{}`)})
	if e := lastOfType(drain(t, a), protocol.TypeError); e == nil {
		t.Fatal("cross-room rtc signal was not rejected")
	}
	if len(drain(t, c)) != 0 {
		t.Fatal("cross-room rtc signal leaked")
	}

	big := json.RawMessage(`"` + strings.Repeat("s", protocol.MaxSignalBytes) + `"`)
	sendText(h, a, protocol.TypeRTC, protocol.RTC{To: "b", Signal: big})
	if e := lastOfType(drain(t, a), protocol.TypeError); e == nil {
		t.Fatal("oversized rtc signal was not rejected")
	}
}

func TestEncryptedOfferNegotiation(t *testing.T) {
	h := testHub()
	a := addTestClient(h, "a", "1.1.1.1")
	b := addTestClient(h, "b", "1.1.1.1")
	drain(t, a)
	drain(t, b)

	id := uuid.New()
	sendText(h, a, protocol.TypeTransferOffer, protocol.TransferOffer{ID: id.String(), To: "b", Name: "x.bin", Size: protocol.ChunkSize + 1, Key: "pubA"})
	var offer protocol.TransferOffer
	must(t, json.Unmarshal(lastOfType(drain(t, b), protocol.TypeTransferOffer).Data, &offer))
	if offer.Key != "pubA" {
		t.Fatalf("offer key not forwarded: %+v", offer)
	}
	sendText(h, b, protocol.TypeTransferAnswer, protocol.TransferAnswer{ID: id.String(), Accept: true, Key: "pubB"})
	var ans protocol.TransferAnswer
	must(t, json.Unmarshal(lastOfType(drain(t, a), protocol.TypeTransferAnswer).Data, &ans))
	if ans.Key != "pubB" {
		t.Fatalf("answer key not forwarded: %+v", ans)
	}
	tr := h.transfers[id]
	if want := protocol.WireSize(protocol.ChunkSize + 1); tr.Wire != want {
		t.Fatalf("Wire = %d, want %d", tr.Wire, want)
	}
	buf := make([]byte, protocol.MaxFrameSize)
	h.handleFrame(inbound{from: a, frame: protocol.EncodeFrame(buf, id, make([]byte, protocol.ChunkSize+protocol.TagBytes)), pool: &buf})
	if tr.State != StateActive {
		t.Fatalf("tagged chunk rejected, state = %s", tr.State)
	}

	id2 := uuid.New()
	sendText(h, a, protocol.TypeTransferOffer, protocol.TransferOffer{ID: id2.String(), To: "b", Name: "y.bin", Size: 10})
	drain(t, b)
	sendText(h, b, protocol.TypeTransferAnswer, protocol.TransferAnswer{ID: id2.String(), Accept: true, Key: "pubB"})
	var ans2 protocol.TransferAnswer
	must(t, json.Unmarshal(lastOfType(drain(t, a), protocol.TypeTransferAnswer).Data, &ans2))
	if ans2.Key != "" || h.transfers[id2].Wire != 10 {
		t.Fatalf("plaintext offer got a key back: %+v wire=%d", ans2, h.transfers[id2].Wire)
	}
}
