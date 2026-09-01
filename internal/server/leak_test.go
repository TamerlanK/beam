package server

import (
	"testing"
	"time"

	"github.com/TamerlanK/beam/internal/protocol"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"go.uber.org/goleak"
)

func TestNoGoroutineLeaks(t *testing.T) {
	defer goleak.VerifyNone(t)
	addr, stop := startServer(t)

	for range 50 {
		s := dial(t, addr)
		r := dial(t, addr)

		payload := randomPayload(t, 3*protocol.ChunkSize)
		id := uuid.New()
		s.send(protocol.TypeTransferOffer, protocol.TransferOffer{
			ID: id.String(), To: r.self.ID, Name: "cycle.bin", Size: int64(len(payload)),
		})
		env := r.expect(protocol.TypeTransferOffer)
		var offer protocol.TransferOffer
		r.unmarshal(env, &offer)
		r.send(protocol.TypeTransferAnswer, protocol.TransferAnswer{ID: offer.ID, Accept: true})
		s.expect(protocol.TypeTransferAnswer)

		buf := make([]byte, protocol.MaxFrameSize)
		for off := 0; off < len(payload); off += protocol.ChunkSize {
			frame := protocol.EncodeFrame(buf, id, payload[off:off+protocol.ChunkSize])
			if err := s.conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
				t.Fatalf("write chunk: %v", err)
			}
		}
		var got int
		for got < len(payload) {
			_, frame := r.next(10 * time.Second)
			if frame != nil {
				got += len(frame) - protocol.FrameOverhead
			}
		}
		r.expect(protocol.TypeTransferComplete)
		s.expect(protocol.TypeTransferComplete)

		s.close()
		r.close()
	}

	stop()
}
