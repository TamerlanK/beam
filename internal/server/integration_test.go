package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/TamerlanK/beam/internal/protocol"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

func startServer(t *testing.T) (addr string, stop func()) {
	t.Helper()
	s, err := New(Config{
		Addr: "127.0.0.1:0",
		Log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	return s.Addr(), func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("server Run: %v", err)
		}
	}
}

type wsClient struct {
	t    *testing.T
	conn *websocket.Conn
	self protocol.Peer
}

func dial(t *testing.T, addr string) *wsClient {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial("ws://"+addr+"/ws", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c := &wsClient{t: t, conn: conn}
	env := c.expect(protocol.TypeRoomState)
	var rs protocol.RoomState
	c.unmarshal(env, &rs)
	c.self = rs.Self
	return c
}

func (c *wsClient) close() { _ = c.conn.Close() }

func (c *wsClient) send(msgType string, data any) {
	c.t.Helper()
	b, err := protocol.Encode(msgType, data)
	if err != nil {
		c.t.Fatalf("encode %s: %v", msgType, err)
	}
	if err := c.conn.WriteMessage(websocket.TextMessage, b); err != nil {
		c.t.Fatalf("write %s: %v", msgType, err)
	}
}

func (c *wsClient) next(timeout time.Duration) (env protocol.Envelope, frame []byte) {
	c.t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(timeout))
	typ, data, err := c.conn.ReadMessage()
	if err != nil {
		c.t.Fatalf("read: %v", err)
	}
	if typ == websocket.BinaryMessage {
		return protocol.Envelope{}, data
	}
	env, err = protocol.Decode(data)
	if err != nil {
		c.t.Fatalf("bad envelope from server: %v", err)
	}
	return env, nil
}

func (c *wsClient) expect(msgType string) protocol.Envelope {
	c.t.Helper()
	for range 64 {
		env, frame := c.next(10 * time.Second)
		if frame != nil {
			continue
		}
		if env.Type == msgType {
			return env
		}
		if env.Type == protocol.TypeError || env.Type == protocol.TypeTransferFailed {
			c.t.Fatalf("expected %s, got %s: %s", msgType, env.Type, env.Data)
		}
	}
	c.t.Fatalf("no %s in 64 messages", msgType)
	return protocol.Envelope{}
}

func (c *wsClient) unmarshal(env protocol.Envelope, v any) {
	c.t.Helper()
	if err := protocol.UnmarshalData(env, v); err != nil {
		c.t.Fatalf("unmarshal %s: %v", env.Type, err)
	}
}

func (c *wsClient) stream(id uuid.UUID, payload []byte) (credits int) {
	c.t.Helper()
	window := protocol.CreditWindow
	offset := 0
	buf := make([]byte, protocol.MaxFrameSize)
	for offset < len(payload) {
		if window > 0 {
			end := min(offset+protocol.ChunkSize, len(payload))
			frame := protocol.EncodeFrame(buf, id, payload[offset:end])
			if err := c.conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
				c.t.Fatalf("write chunk: %v", err)
			}
			window--
			offset = end
			continue
		}
		env, _ := c.next(10 * time.Second)
		switch env.Type {
		case protocol.TypeFlowCredit:
			var fc protocol.FlowCredit
			c.unmarshal(env, &fc)
			window += fc.N
			credits += fc.N
		case protocol.TypeTransferFailed, protocol.TypeError:
			c.t.Fatalf("transfer failed while streaming: %s", env.Data)
		}
	}
	for {
		env, _ := c.next(10 * time.Second)
		switch env.Type {
		case protocol.TypeFlowCredit:
			credits++
		case protocol.TypeTransferComplete:
			var tc protocol.TransferComplete
			c.unmarshal(env, &tc)
			if tc.Bytes != int64(len(payload)) {
				c.t.Fatalf("complete reports %d bytes, sent %d", tc.Bytes, len(payload))
			}
			return credits
		case protocol.TypeTransferFailed, protocol.TypeError:
			c.t.Fatalf("transfer failed at the end: %s", env.Data)
		}
	}
}

func (c *wsClient) receive() (name string, sum string) {
	c.t.Helper()
	env := c.expect(protocol.TypeTransferOffer)
	var offer protocol.TransferOffer
	c.unmarshal(env, &offer)
	c.send(protocol.TypeTransferAnswer, protocol.TransferAnswer{ID: offer.ID, Accept: true})

	h := sha256.New()
	var got int64
	for {
		env, frame := c.next(30 * time.Second)
		if frame != nil {
			id, chunk, err := protocol.SplitFrame(frame)
			if err != nil {
				c.t.Fatalf("bad frame: %v", err)
			}
			if id.String() != offer.ID {
				c.t.Fatalf("frame for wrong transfer %s", id)
			}
			h.Write(chunk)
			got += int64(len(chunk))
			continue
		}
		if env.Type == protocol.TypeTransferComplete {
			if got != offer.Size {
				c.t.Fatalf("received %d bytes, offer said %d", got, offer.Size)
			}
			return offer.Name, hex.EncodeToString(h.Sum(nil))
		}
		if env.Type == protocol.TypeTransferFailed || env.Type == protocol.TypeError {
			c.t.Fatalf("receive failed: %s", env.Data)
		}
	}
}

func randomPayload(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestIntegration50MBTransfer(t *testing.T) {
	if testing.Short() {
		t.Skip("50MB stream skipped in -short")
	}
	addr, stop := startServer(t)
	defer stop()

	sender := dial(t, addr)
	defer sender.close()
	receiver := dial(t, addr)
	defer receiver.close()
	sender.expect(protocol.TypePeerJoined)

	payload := randomPayload(t, 50<<20)
	wantSum := sha256.Sum256(payload)

	id := uuid.New()
	sender.send(protocol.TypeTransferOffer, protocol.TransferOffer{
		ID: id.String(), To: receiver.self.ID, Name: "big.bin", Size: int64(len(payload)),
	})

	type recvResult struct {
		name, sum string
	}
	recvDone := make(chan recvResult, 1)
	go func() {
		name, sum := receiver.receive()
		recvDone <- recvResult{name, sum}
	}()

	env := sender.expect(protocol.TypeTransferAnswer)
	var ans protocol.TransferAnswer
	sender.unmarshal(env, &ans)
	if !ans.Accept {
		t.Fatal("offer was declined")
	}

	credits := sender.stream(id, payload)
	res := <-recvDone

	if res.sum != hex.EncodeToString(wantSum[:]) {
		t.Fatal("SHA-256 mismatch: relayed bytes corrupted")
	}
	if res.name != "big.bin" {
		t.Fatalf("received name %q", res.name)
	}

	if credits < 700 {
		t.Fatalf("only %d flow credits for an 800-chunk transfer", credits)
	}
}

func TestConcurrentPairs(t *testing.T) {
	addr, stop := startServer(t)
	defer stop()

	const pairs = 2
	type result struct {
		want, got string
	}
	results := make(chan result, pairs)
	for i := range pairs {
		go func(i int) {
			s := dial(t, addr)
			defer s.close()
			r := dial(t, addr)
			defer r.close()

			payload := randomPayload(t, 2<<20+i*17)
			want := sha256.Sum256(payload)
			id := uuid.New()
			s.send(protocol.TypeTransferOffer, protocol.TransferOffer{
				ID: id.String(), To: r.self.ID, Name: fmt.Sprintf("p%d.bin", i), Size: int64(len(payload)),
			})
			sumc := make(chan string, 1)
			go func() {
				_, sum := r.receive()
				sumc <- sum
			}()
			s.expect(protocol.TypeTransferAnswer)
			s.stream(id, payload)
			results <- result{hex.EncodeToString(want[:]), <-sumc}
		}(i)
	}
	for range pairs {
		select {
		case res := <-results:
			if res.got != res.want {
				t.Fatal("concurrent transfer corrupted")
			}
		case <-time.After(60 * time.Second):
			t.Fatal("concurrent transfers timed out")
		}
	}
}

func TestDisconnectMidTransferFailsOnlyOwnTransfer(t *testing.T) {
	addr, stop := startServer(t)
	defer stop()

	s1 := dial(t, addr)
	r1 := dial(t, addr)
	defer r1.close()
	s2 := dial(t, addr)
	defer s2.close()
	r2 := dial(t, addr)
	defer r2.close()

	id1 := uuid.New()
	s1.send(protocol.TypeTransferOffer, protocol.TransferOffer{
		ID: id1.String(), To: r1.self.ID, Name: "doomed.bin", Size: 10 << 20,
	})
	env := r1.expect(protocol.TypeTransferOffer)
	var offer protocol.TransferOffer
	r1.unmarshal(env, &offer)
	r1.send(protocol.TypeTransferAnswer, protocol.TransferAnswer{ID: offer.ID, Accept: true})
	s1.expect(protocol.TypeTransferAnswer)
	buf := make([]byte, protocol.MaxFrameSize)
	if err := s1.conn.WriteMessage(websocket.BinaryMessage,
		protocol.EncodeFrame(buf, id1, make([]byte, protocol.ChunkSize))); err != nil {
		t.Fatal(err)
	}
	s1.close()

	payload := randomPayload(t, 4<<20)
	want := sha256.Sum256(payload)
	id2 := uuid.New()
	s2.send(protocol.TypeTransferOffer, protocol.TransferOffer{
		ID: id2.String(), To: r2.self.ID, Name: "fine.bin", Size: int64(len(payload)),
	})
	sumc := make(chan string, 1)
	go func() {
		_, sum := r2.receive()
		sumc <- sum
	}()
	s2.expect(protocol.TypeTransferAnswer)
	s2.stream(id2, payload)
	if sum := <-sumc; sum != hex.EncodeToString(want[:]) {
		t.Fatal("healthy pair corrupted by other pair's disconnect")
	}

	for {
		env, frame := r1.next(10 * time.Second)
		if frame != nil {
			continue
		}
		if env.Type == protocol.TypeTransferFailed {
			var tf protocol.TransferFailed
			r1.unmarshal(env, &tf)
			if tf.Reason != "peer-disconnected" {
				t.Fatalf("reason = %q", tf.Reason)
			}
			return
		}
	}
}

func TestServerShutdownNotifiesActiveTransfers(t *testing.T) {
	addr, stop := startServer(t)

	s := dial(t, addr)
	defer s.close()
	r := dial(t, addr)
	defer r.close()

	id := uuid.New()
	s.send(protocol.TypeTransferOffer, protocol.TransferOffer{
		ID: id.String(), To: r.self.ID, Name: "cut.bin", Size: 10 << 20,
	})
	env := r.expect(protocol.TypeTransferOffer)
	var offer protocol.TransferOffer
	r.unmarshal(env, &offer)
	r.send(protocol.TypeTransferAnswer, protocol.TransferAnswer{ID: offer.ID, Accept: true})
	s.expect(protocol.TypeTransferAnswer)

	stop()

	env = s.expect(protocol.TypeTransferFailed)
	var tf protocol.TransferFailed
	s.unmarshal(env, &tf)
	if tf.Reason != "server-shutdown" {
		t.Fatalf("reason = %q, want server-shutdown", tf.Reason)
	}
}
