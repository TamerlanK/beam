package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"github.com/TamerlanK/beam/internal/protocol"
	"github.com/TamerlanK/beam/internal/server"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

func main() {
	clients := flag.Int("clients", 200, "total websocket clients")
	rooms := flag.Int("rooms", 50, "rooms to spread clients across")
	transfers := flag.Int("transfers", 25, "concurrent transfers")
	size := flag.Int64("size", 20<<20, "payload bytes per transfer")
	addr := flag.String("addr", "", "external server address (empty: start one in-process)")
	flag.Parse()

	if *rooms > *clients/2 && *transfers > 0 {

		fmt.Fprintln(os.Stderr, "need at least 2 clients per room")
		os.Exit(1)
	}
	if *transfers > *rooms {
		fmt.Fprintln(os.Stderr, "transfers must be <= rooms (one transfer per room)")
		os.Exit(1)
	}

	target := *addr
	var stop func()
	if target == "" {
		var err error
		target, stop, err = startServer()
		if err != nil {
			fmt.Fprintln(os.Stderr, "start server:", err)
			os.Exit(1)
		}
	}

	fmt.Printf("beam loadtest: %d clients, %d rooms, %d concurrent %s transfers → %s\n\n",
		*clients, *rooms, *transfers, fmtBytes(*size), target)

	memDone := make(chan struct{})
	var peak peakMem
	go peak.sample(memDone)

	pool := make([]*ltClient, *clients)
	var wg sync.WaitGroup
	var dialErr atomic.Value
	for i := range pool {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := dialClient(target, i%*rooms)
			if err != nil {
				dialErr.Store(err)
				return
			}
			pool[i] = c
		}(i)
	}
	wg.Wait()
	if err := dialErr.Load(); err != nil {
		fmt.Fprintln(os.Stderr, "dial:", err)
		os.Exit(1)
	}
	defer func() {
		for _, c := range pool {
			c.close()
		}
		if stop != nil {
			stop()
		}
	}()

	byRoom := make([][]*ltClient, *rooms)
	for i, c := range pool {
		byRoom[i%*rooms] = append(byRoom[i%*rooms], c)
	}

	start := time.Now()
	results := make(chan xferResult, *transfers)
	for i := range *transfers {
		go runTransfer(byRoom[i][0], byRoom[i][1], *size, results)
	}

	var lats []time.Duration
	var totalBytes int64
	var perXferMBps []float64
	fails := 0
	for range *transfers {
		res := <-results
		if res.err != nil {
			fails++
			fmt.Fprintln(os.Stderr, "transfer failed:", res.err)
			continue
		}
		totalBytes += res.bytes
		perXferMBps = append(perXferMBps, float64(res.bytes)/(1<<20)/res.dur.Seconds())
		lats = append(lats, res.lats...)
	}
	wall := time.Since(start)
	close(memDone)

	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	var avg float64
	for _, v := range perXferMBps {
		avg += v
	}
	if len(perXferMBps) > 0 {
		avg /= float64(len(perXferMBps))
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "wall time\t%s\n", wall.Round(time.Millisecond))
	_, _ = fmt.Fprintf(w, "completed transfers\t%d/%d\n", *transfers-fails, *transfers)
	_, _ = fmt.Fprintf(w, "total relayed\t%s\n", fmtBytes(totalBytes))
	_, _ = fmt.Fprintf(w, "total throughput\t%.1f MB/s\n", float64(totalBytes)/(1<<20)/wall.Seconds())
	_, _ = fmt.Fprintf(w, "per-transfer avg\t%.1f MB/s\n", avg)
	_, _ = fmt.Fprintf(w, "relay latency p50\t%s\n", percentile(lats, 50))
	_, _ = fmt.Fprintf(w, "relay latency p99\t%s\n", percentile(lats, 99))
	_, _ = fmt.Fprintf(w, "peak memory\t%s\t(%s)\n", fmtBytes(int64(peak.load())), peak.method())
	_ = w.Flush()
	if fails > 0 {
		os.Exit(1)
	}
}

func startServer() (addr string, stop func(), err error) {
	s, err := server.New(server.Config{
		Addr:       "127.0.0.1:0",
		TrustProxy: true,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		return "", nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Run(ctx)
	}()
	return s.Addr(), func() { cancel(); <-done }, nil
}

type ltClient struct {
	conn  *websocket.Conn
	self  protocol.Peer
	peers chan protocol.Peer
	ctrl  chan protocol.Envelope
	bin   chan []byte
	dead  chan struct{}
}

func dialClient(addr string, room int) (*ltClient, error) {
	h := http.Header{}

	h.Set("X-Forwarded-For", fmt.Sprintf("10.77.%d.%d", room/250, room%250+1))
	conn, _, err := websocket.DefaultDialer.Dial("ws://"+addr+"/ws", h)
	if err != nil {
		return nil, err
	}
	c := &ltClient{
		conn:  conn,
		peers: make(chan protocol.Peer, 64),
		ctrl:  make(chan protocol.Envelope, 256),
		bin:   make(chan []byte, protocol.CreditWindow*2),
		dead:  make(chan struct{}),
	}

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	env, err := protocol.Decode(data)
	if err != nil || env.Type != protocol.TypeRoomState {
		_ = conn.Close()
		return nil, fmt.Errorf("expected room-state, got %q (%v)", env.Type, err)
	}
	var rs protocol.RoomState
	if err := protocol.UnmarshalData(env, &rs); err != nil {
		_ = conn.Close()
		return nil, err
	}
	c.self = rs.Self

	go c.readLoop()
	return c, nil
}

func (c *ltClient) readLoop() {
	defer close(c.dead)
	_ = c.conn.SetReadDeadline(time.Time{})
	for {
		typ, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		if typ == websocket.BinaryMessage {
			select {
			case c.bin <- data:
			case <-time.After(30 * time.Second):
				return
			}
			continue
		}
		env, err := protocol.Decode(data)
		if err != nil {
			continue
		}
		switch env.Type {
		case protocol.TypePeerJoined:
			var p protocol.Peer
			if protocol.UnmarshalData(env, &p) == nil {
				select {
				case c.peers <- p:
				default:
				}
			}
		case protocol.TypeFlowCredit, protocol.TypeTransferOffer, protocol.TypeTransferAnswer,
			protocol.TypeTransferComplete, protocol.TypeTransferFailed, protocol.TypeError:
			select {
			case c.ctrl <- env:
			case <-time.After(30 * time.Second):
				return
			}
		}
	}
}

func (c *ltClient) close() {
	if c != nil {
		_ = c.conn.Close()
	}
}

func (c *ltClient) send(msgType string, data any) error {
	b, err := protocol.Encode(msgType, data)
	if err != nil {
		return err
	}
	return c.conn.WriteMessage(websocket.TextMessage, b)
}

type xferResult struct {
	bytes int64
	dur   time.Duration
	lats  []time.Duration
	err   error
}

func runTransfer(s, r *ltClient, size int64, out chan<- xferResult) {
	res := xferResult{}
	defer func() { out <- res }()

	id := uuid.New()
	if err := s.send(protocol.TypeTransferOffer, protocol.TransferOffer{
		ID: id.String(), To: r.self.ID, Name: "load.bin", Size: size,
	}); err != nil {
		res.err = err
		return
	}

	recvDone := make(chan error, 1)
	go func() { recvDone <- receiveAll(r, id, size, &res) }()

	if err := awaitCtrl(s, id, protocol.TypeTransferAnswer); err != nil {
		res.err = err
		return
	}

	chunk := make([]byte, protocol.ChunkSize)
	if _, err := rand.Read(chunk); err != nil {
		res.err = err
		return
	}
	frame := make([]byte, protocol.MaxFrameSize)
	window := protocol.CreditWindow
	begin := time.Now()
	var sent int64
	for sent < size {
		if window == 0 {
			select {
			case env := <-s.ctrl:
				n, err := creditFor(env, id)
				if err != nil {
					res.err = err
					return
				}
				window += n
			case <-s.dead:
				res.err = fmt.Errorf("sender died")
				return
			}
			continue
		}
		n := int64(protocol.ChunkSize)
		if size-sent < n {
			n = size - sent
		}
		binary.BigEndian.PutUint64(chunk, uint64(time.Now().UnixNano()))
		f := protocol.EncodeFrame(frame, id, chunk[:n])
		if err := s.conn.WriteMessage(websocket.BinaryMessage, f); err != nil {
			res.err = err
			return
		}
		window--
		sent += n
	}

	for {
		select {
		case env := <-s.ctrl:
			if env.Type == protocol.TypeTransferComplete {
				if err := <-recvDone; err != nil {
					res.err = err
					return
				}
				res.bytes = size
				res.dur = time.Since(begin)
				return
			}
			if env.Type == protocol.TypeTransferFailed || env.Type == protocol.TypeError {
				res.err = fmt.Errorf("sender saw %s: %s", env.Type, env.Data)
				return
			}
		case <-time.After(2 * time.Minute):
			res.err = fmt.Errorf("timeout waiting for completion")
			return
		}
	}
}

func receiveAll(r *ltClient, id uuid.UUID, size int64, res *xferResult) error {

	if err := awaitCtrl(r, id, protocol.TypeTransferOffer); err != nil {
		return err
	}
	if err := r.send(protocol.TypeTransferAnswer, protocol.TransferAnswer{ID: id.String(), Accept: true}); err != nil {
		return err
	}
	var got int64
	for got < size {
		select {
		case frame := <-r.bin:
			fid, chunk, err := protocol.SplitFrame(frame)
			if err != nil {
				return err
			}
			if fid != id {
				continue
			}
			if len(chunk) >= 8 {
				sentAt := time.Unix(0, int64(binary.BigEndian.Uint64(chunk)))
				res.lats = append(res.lats, time.Since(sentAt))
			}
			got += int64(len(chunk))
		case <-r.dead:
			return fmt.Errorf("receiver died")
		case <-time.After(2 * time.Minute):
			return fmt.Errorf("receiver timeout at %d/%d bytes", got, size)
		}
	}
	return awaitCtrl(r, id, protocol.TypeTransferComplete)
}

func awaitCtrl(c *ltClient, id uuid.UUID, msgType string) error {
	for {
		select {
		case env := <-c.ctrl:
			switch env.Type {
			case msgType:
				return nil
			case protocol.TypeTransferFailed, protocol.TypeError:
				return fmt.Errorf("got %s: %s", env.Type, env.Data)
			}
		case <-c.dead:
			return fmt.Errorf("connection closed waiting for %s", msgType)
		case <-time.After(2 * time.Minute):
			return fmt.Errorf("timeout waiting for %s", msgType)
		}
	}
}

func creditFor(env protocol.Envelope, id uuid.UUID) (int, error) {
	switch env.Type {
	case protocol.TypeFlowCredit:
		var fc protocol.FlowCredit
		if err := protocol.UnmarshalData(env, &fc); err != nil {
			return 0, err
		}
		return fc.N, nil
	case protocol.TypeTransferFailed, protocol.TypeError:
		return 0, fmt.Errorf("got %s: %s", env.Type, env.Data)
	case protocol.TypeTransferComplete:
		return 0, fmt.Errorf("complete before all chunks sent")
	default:
		return 0, nil
	}
}

func percentile(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := len(sorted) * p / 100
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i].Round(10 * time.Microsecond)
}

func fmtBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
