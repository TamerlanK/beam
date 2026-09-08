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
	return testHubLimits(Limits{})
}

func testHubLimits(l Limits) *Hub {
	return New(slog.New(slog.NewTextHandler(io.Discard, nil)), l)
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

func TestRoomKeyGroupsIPv6ByPrefix(t *testing.T) {
	cases := map[string]string{
		"1.2.3.4":                 "ip:1.2.3.4",
		"::ffff:1.2.3.4":          "ip:1.2.3.4",
		"2001:db8:1:2:aaaa::1":    "ip:2001:db8:1:2::/64",
		"2001:db8:1:2:bbbb:1:2:3": "ip:2001:db8:1:2::/64",
		"2001:db8:1:3::1":         "ip:2001:db8:1:3::/64",
		"10.0.0.7":                "ip:lan",
		"fd12::1":                 "ip:lan",
		"fe80::1":                 "ip:lan",
		"::1":                     "ip:lan",
		"not-an-ip":               "ip:not-an-ip",
	}
	for ip, want := range cases {
		if got := roomKey(ip); got != want {
			t.Errorf("roomKey(%q) = %q, want %q", ip, got, want)
		}
	}

	h := testHub()
	a := addTestClient(h, "a", "2001:db8:1:2:aaaa::1")
	b := addTestClient(h, "b", "2001:db8:1:2:bbbb:1:2:3")
	c := addTestClient(h, "c", "2001:db8:1:3::1")
	if a.room != b.room {
		t.Fatal("same-/64 IPv6 clients not grouped into one room")
	}
	if c.room == a.room {
		t.Fatal("different-/64 IPv6 client landed in the same room")
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

	a.room.codeExpires = time.Now().Add(2 * time.Minute)
	sendText(h, a, protocol.TypeRoomCreate, nil)
	env = lastOfType(drain(t, a), protocol.TypeRoomCreated)
	var rc2 protocol.RoomCreated
	must(t, json.Unmarshal(env.Data, &rc2))
	if rc2.Code != rc.Code || rc2.ExpiresIn > 120 {
		t.Fatalf("re-create changed code or extended TTL: %+v", rc2)
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
	if dup.ID != "a" || h.clients["a"] != dup || h.conns["1.1.1.1"] != 2 {
		t.Fatalf("reconnecting device did not replace the old connection: conns=%v", h.conns)
	}
	replaced := false
	for f := range a.send {
		env, err := protocol.Decode(f.data)
		must(t, err)
		var e protocol.Error
		if env.Type == protocol.TypeError && json.Unmarshal(env.Data, &e) == nil && e.Code == protocol.ErrCodeReplaced {
			replaced = true
		}
	}
	if !replaced {
		t.Fatal("old connection was not told it was replaced before its send channel closed")
	}
	h.removeClient(a)
	if h.clients["a"] != dup {
		t.Fatal("stale disconnect of the old connection removed the new one")
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

	// The receiver may not reveal a key; the sender may, exactly while accepted.
	sendText(h, b, protocol.TypeTransferKey, protocol.TransferKey{ID: id.String(), Key: "pubA"})
	if lastOfType(drain(t, b), protocol.TypeError) == nil {
		t.Fatal("receiver's transfer-key accepted")
	}
	sendText(h, a, protocol.TypeTransferKey, protocol.TransferKey{ID: id.String(), Key: "pubA"})
	var reveal protocol.TransferKey
	must(t, json.Unmarshal(lastOfType(drain(t, b), protocol.TypeTransferKey).Data, &reveal))
	if reveal.ID != id.String() || reveal.Key != "pubA" {
		t.Fatalf("transfer-key not forwarded: %+v", reveal)
	}
	buf := make([]byte, protocol.MaxFrameSize)
	h.handleFrame(inbound{from: a, frame: protocol.EncodeFrame(buf, id, make([]byte, protocol.ChunkSize+protocol.TagBytes)), pool: &buf})
	if tr.State != StateActive {
		t.Fatalf("tagged chunk rejected, state = %s", tr.State)
	}
	drain(t, b)
	sendText(h, a, protocol.TypeTransferKey, protocol.TransferKey{ID: id.String(), Key: "pubA"})
	if lastOfType(drain(t, a), protocol.TypeError) == nil {
		t.Fatal("transfer-key after data accepted")
	}
	if lastOfType(drain(t, b), protocol.TypeTransferKey) != nil {
		t.Fatal("late transfer-key forwarded")
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
	sendText(h, a, protocol.TypeTransferKey, protocol.TransferKey{ID: id2.String(), Key: "pubA"})
	if lastOfType(drain(t, a), protocol.TypeError) == nil {
		t.Fatal("transfer-key on a plaintext transfer accepted")
	}
}

func TestOfferPreviewValidation(t *testing.T) {
	h := testHub()
	a := addTestClient(h, "a", "1.1.1.1")
	b := addTestClient(h, "b", "1.1.1.1")
	drain(t, a)
	drain(t, b)

	offer := func(id uuid.UUID, preview string) {
		sendText(h, a, protocol.TypeTransferOffer, protocol.TransferOffer{ID: id.String(), To: "b", Name: "p.png", Size: 10, Preview: preview})
	}
	offer(uuid.New(), "data:text/html;base64,PGI+")
	if lastOfType(drain(t, a), protocol.TypeError) == nil || len(drain(t, b)) != 0 {
		t.Fatal("non-image preview was not rejected")
	}
	offer(uuid.New(), "data:image/png;base64,"+strings.Repeat("A", protocol.MaxPreviewBytes))
	if lastOfType(drain(t, a), protocol.TypeError) == nil || len(drain(t, b)) != 0 {
		t.Fatal("oversized preview was not rejected")
	}
	id := uuid.New()
	offer(id, "data:image/jpeg;base64,/9j/4AAQ")
	var got protocol.TransferOffer
	must(t, json.Unmarshal(lastOfType(drain(t, b), protocol.TypeTransferOffer).Data, &got))
	if got.Preview != "data:image/jpeg;base64,/9j/4AAQ" {
		t.Fatalf("preview not forwarded: %+v", got)
	}
}

func TestMaxConnsPerIP(t *testing.T) {
	h := testHubLimits(Limits{MaxConnsPerIP: 2})
	a := addTestClient(h, "a", "1.1.1.1")
	b := addTestClient(h, "b", "1.1.1.1")
	addTestClient(h, "z", "2.2.2.2")
	c := addTestClient(h, "c", "1.1.1.1")

	f, ok := <-c.send
	if !ok || f.msgType != websocket.TextMessage {
		t.Fatalf("rejected client got no error frame: ok=%v type=%d", ok, f.msgType)
	}
	env, err := protocol.Decode(f.data)
	must(t, err)
	var e protocol.Error
	must(t, json.Unmarshal(env.Data, &e))
	if env.Type != protocol.TypeError || e.Code != protocol.ErrCodeTooManyConns {
		t.Fatalf("got %s/%s, want error/%s", env.Type, e.Code, protocol.ErrCodeTooManyConns)
	}
	if _, ok := <-c.send; ok {
		t.Fatal("rejected client's send channel was not closed")
	}
	if h.clients["c"] != nil || h.clients["a"] != a || h.clients["b"] != b || h.conns["1.1.1.1"] != 2 {
		t.Fatalf("clients=%d conns=%v", len(h.clients), h.conns)
	}

	h.removeClient(c)
	if h.conns["1.1.1.1"] != 2 {
		t.Fatal("removing a rejected client changed the count")
	}
	h.removeClient(a)
	d := addTestClient(h, "d", "1.1.1.1")
	if h.clients["d"] != d || h.conns["1.1.1.1"] != 2 {
		t.Fatalf("slot not freed after disconnect: conns=%v", h.conns)
	}
}

func TestRelayBudgetThrottlesCredits(t *testing.T) {
	h := testHubLimits(Limits{RelayBytesPerSec: protocol.ChunkSize})
	a := addTestClient(h, "a", "1.1.1.1")
	b := addTestClient(h, "b", "1.1.1.1")
	drain(t, a)
	drain(t, b)

	id := uuid.New()
	sendText(h, a, protocol.TypeTransferOffer, protocol.TransferOffer{ID: id.String(), To: "b", Name: "x.bin", Size: 4 * protocol.ChunkSize})
	sendText(h, b, protocol.TypeTransferAnswer, protocol.TransferAnswer{ID: id.String(), Accept: true})
	drain(t, a)
	drain(t, b)

	ticks := 0
	for range 4 {
		buf := make([]byte, protocol.MaxFrameSize)
		h.handleFrame(inbound{from: a, frame: protocol.EncodeFrame(buf, id, make([]byte, protocol.ChunkSize)), pool: &buf})
		<-b.send
		h.handleWritten(written{to: b, id: id, n: protocol.ChunkSize})
		for lastOfType(drain(t, a), protocol.TypeFlowCredit) == nil && h.transfers[id] != nil {
			if ticks++; ticks > 10 {
				t.Fatal("credit never arrived")
			}
			h.tick(time.Now())
		}
	}
	if h.transfers[id] != nil || lastOfType(drain(t, b), protocol.TypeTransferComplete) == nil {
		t.Fatal("throttled transfer did not complete")
	}
	if ticks < 3 {
		t.Fatalf("four chunks at one chunk per second took %d ticks, want at least 3", ticks)
	}
	if len(h.starved) != 0 {
		t.Fatalf("starved queue not drained: %d", len(h.starved))
	}
}

func relayChunk(t *testing.T, h *Hub, from, to *Client, id uuid.UUID, n int) {
	t.Helper()
	buf := make([]byte, protocol.MaxFrameSize)
	h.handleFrame(inbound{from: from, frame: protocol.EncodeFrame(buf, id, make([]byte, n)), pool: &buf})
	select {
	case f := <-to.send:
		if f.msgType != websocket.BinaryMessage {
			t.Fatalf("expected a relayed chunk, got msgType %d", f.msgType)
		}
	default:
		t.Fatal("chunk was not relayed")
	}
	h.handleWritten(written{to: to, id: id, n: n})
}

func TestResumeRelaysOnlyRemainder(t *testing.T) {
	h := testHub()
	a := addTestClient(h, "a", "1.1.1.1")
	b := addTestClient(h, "b", "1.1.1.1")
	drain(t, a)
	drain(t, b)

	id := uuid.New()
	size := int64(4 * protocol.ChunkSize)
	sendText(h, a, protocol.TypeTransferOffer, protocol.TransferOffer{ID: id.String(), To: "b", Name: "x.bin", Size: size, Key: "pubA"})
	drain(t, b)
	sendText(h, b, protocol.TypeTransferAnswer, protocol.TransferAnswer{ID: id.String(), Accept: true, Key: "pubB"})
	drain(t, a)
	relayChunk(t, h, a, b, id, protocol.ChunkSize+protocol.TagBytes)
	relayChunk(t, h, a, b, id, protocol.ChunkSize+protocol.TagBytes)
	drain(t, a)

	h.removeClient(b)
	var failed protocol.TransferFailed
	must(t, json.Unmarshal(lastOfType(drain(t, a), protocol.TypeTransferFailed).Data, &failed))
	if failed.Reason != "peer-disconnected" || h.transfers[id] != nil {
		t.Fatalf("drop did not fail the transfer: %+v", failed)
	}

	b2 := addTestClient(h, "b", "1.1.1.1")
	drain(t, a)
	drain(t, b2)
	hint := int64(3 * protocol.ChunkSize)
	sendText(h, a, protocol.TypeTransferOffer, protocol.TransferOffer{ID: id.String(), To: "b", Name: "x.bin", Size: size, Key: "pubA", Offset: hint})
	var offer protocol.TransferOffer
	must(t, json.Unmarshal(lastOfType(drain(t, b2), protocol.TypeTransferOffer).Data, &offer))
	if offer.Offset != hint {
		t.Fatalf("offer offset = %d, want hint %d", offer.Offset, hint)
	}

	sendText(h, b2, protocol.TypeTransferAnswer, protocol.TransferAnswer{ID: id.String(), Accept: true, Key: "pubB", Offset: hint + protocol.ChunkSize})
	if lastOfType(drain(t, b2), protocol.TypeError) == nil || h.transfers[id].State != StateOffered {
		t.Fatal("answer above the sender's hint was accepted")
	}
	sendText(h, b2, protocol.TypeTransferAnswer, protocol.TransferAnswer{ID: id.String(), Accept: true, Key: "pubB", Offset: 1})
	if lastOfType(drain(t, b2), protocol.TypeError) == nil || h.transfers[id].State != StateOffered {
		t.Fatal("unaligned answer offset was accepted")
	}

	committed := int64(2 * protocol.ChunkSize)
	sendText(h, b2, protocol.TypeTransferAnswer, protocol.TransferAnswer{ID: id.String(), Accept: true, Key: "pubB", Offset: committed})
	var ans protocol.TransferAnswer
	must(t, json.Unmarshal(lastOfType(drain(t, a), protocol.TypeTransferAnswer).Data, &ans))
	if ans.Offset != committed {
		t.Fatalf("answer offset forwarded as %d, want %d", ans.Offset, committed)
	}
	tr := h.transfers[id]
	if want := protocol.WireSize(committed); tr.Written != want || tr.Relayed != want {
		t.Fatalf("baseline = %d/%d, want %d", tr.Relayed, tr.Written, want)
	}

	h.handleWritten(written{to: b, id: id, n: protocol.ChunkSize})
	if tr.Written != protocol.WireSize(committed) {
		t.Fatal("a stale write from the replaced connection was counted")
	}

	relayChunk(t, h, a, b2, id, protocol.ChunkSize+protocol.TagBytes)
	if tr.State != StateActive {
		t.Fatalf("state after first resumed chunk = %s", tr.State)
	}
	relayChunk(t, h, a, b2, id, protocol.ChunkSize+protocol.TagBytes)
	if h.transfers[id] != nil || lastOfType(drain(t, a), protocol.TypeTransferComplete) == nil || lastOfType(drain(t, b2), protocol.TypeTransferComplete) == nil {
		t.Fatal("resumed transfer did not complete after the remaining two chunks")
	}

	sendText(h, a, protocol.TypeTransferOffer, protocol.TransferOffer{ID: uuid.NewString(), To: "b", Name: "y.bin", Size: size, Offset: size})
	if lastOfType(drain(t, a), protocol.TypeError) == nil {
		t.Fatal("offer with offset at the declared size was accepted")
	}
}
