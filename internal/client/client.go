package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/TamerlanK/beam/internal/names"
	"github.com/TamerlanK/beam/internal/protocol"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	writeWait     = 10 * time.Second
	pongWait      = 60 * time.Second
	handshakeWait = 10 * time.Second
	maxBackoff    = 15 * time.Second
)

type Identity struct {
	ID    string `json:"-"`
	Name  string `json:"name"`
	Emoji string `json:"emoji"`
}

func identityPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "beam", "identity.json"), nil
}

func LoadIdentity() (Identity, error) {
	var id Identity
	path, err := identityPath()
	if err == nil {
		if b, rerr := os.ReadFile(path); rerr == nil {
			_ = json.Unmarshal(b, &id)
		}
	}
	id.ID = uuid.NewString()
	id.Name, id.Emoji = names.CleanName(id.Name), names.CleanEmoji(id.Emoji)
	dirty := false
	if id.Name == "" || id.Emoji == "" {
		n, e := names.Random()
		if id.Name == "" {
			id.Name = n
		}
		if id.Emoji == "" {
			id.Emoji = e
		}
		dirty = true
	}
	if !dirty || err != nil {
		return id, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return id, err
	}
	b, _ := json.MarshalIndent(id, "", "  ")
	return id, os.WriteFile(path, b, 0o600)
}

type Config struct {
	Server    string
	Identity  Identity
	Join      string
	UserAgent string
}

type Event struct {
	Type  string
	Data  json.RawMessage
	Frame []byte
	Err   error
}

const (
	Connected    = "connected"
	Disconnected = "disconnected"
	Frame        = "frame"
)

type FatalError struct{ Code, Message string }

func (e *FatalError) Error() string { return e.Message }

var ErrNotConnected = errors.New("not connected")

type Client struct {
	cfg    Config
	wsURL  string
	Events chan Event

	mu   sync.Mutex
	conn *websocket.Conn

	pmu   sync.RWMutex
	self  protocol.Peer
	peers map[string]protocol.Peer
}

func New(cfg Config) (*Client, error) {
	raw := cfg.Server
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("bad server URL %q", cfg.Server)
	}
	switch u.Scheme {
	case "http", "ws":
		u.Scheme = "ws"
	case "https", "wss":
		u.Scheme = "wss"
	default:
		return nil, fmt.Errorf("server URL must be http(s):// or ws(s)://, got %q", cfg.Server)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/ws"
	q := url.Values{}
	q.Set("id", cfg.Identity.ID)
	q.Set("name", cfg.Identity.Name)
	q.Set("emoji", cfg.Identity.Emoji)
	u.RawQuery = q.Encode()
	u.Fragment = ""
	return &Client{cfg: cfg, wsURL: u.String(), Events: make(chan Event, 256), peers: map[string]protocol.Peer{}}, nil
}

func (c *Client) Run(ctx context.Context) error {
	defer close(c.Events)
	delay := time.Second
	ever := false
	for {
		ok, err := c.session(ctx)
		ever = ever || ok
		if ok {
			delay = time.Second
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var fatal *FatalError
		if !ever || errors.As(err, &fatal) {
			return err
		}

		if ok && !c.emit(ctx, Event{Type: Disconnected, Err: err}) {
			return ctx.Err()
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
		delay = min(delay*2, maxBackoff)
	}
}

func (c *Client) session(ctx context.Context) (ok bool, err error) {
	dialer := websocket.Dialer{HandshakeTimeout: handshakeWait}
	h := http.Header{}
	if c.cfg.UserAgent != "" {
		h.Set("User-Agent", c.cfg.UserAgent)
	}
	conn, resp, err := dialer.DialContext(ctx, c.wsURL, h)
	if err != nil {
		if resp != nil {
			return false, fmt.Errorf("%s: %s", c.cfg.Server, resp.Status)
		}
		return false, err
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	rs, raw, err := c.handshake(conn)
	if err != nil {
		return false, err
	}
	c.setRoom(rs)
	c.setConn(conn)
	defer c.setConn(nil)
	if !c.emit(ctx, Event{Type: Connected, Data: raw}) {
		return true, ctx.Err()
	}

	_ = conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPingHandler(func(s string) error {
		_ = conn.SetReadDeadline(time.Now().Add(pongWait))
		return conn.WriteControl(websocket.PongMessage, []byte(s), time.Now().Add(writeWait))
	})
	for {
		typ, data, err := conn.ReadMessage()
		if err != nil {
			return true, err
		}
		_ = conn.SetReadDeadline(time.Now().Add(pongWait))
		if typ == websocket.BinaryMessage {
			if !c.emit(ctx, Event{Type: Frame, Frame: data}) {
				return true, ctx.Err()
			}
			continue
		}
		env, err := protocol.Decode(data)
		if err != nil {
			continue
		}
		if fatal := c.track(env); fatal != nil {
			return true, fatal
		}
		if !c.emit(ctx, Event{Type: env.Type, Data: env.Data}) {
			return true, ctx.Err()
		}
	}
}

func (c *Client) handshake(conn *websocket.Conn) (protocol.RoomState, json.RawMessage, error) {
	rs, raw, err := readRoom(conn, "")
	if err != nil || c.cfg.Join == "" {
		return rs, raw, err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
	join := protocol.MustEncode(protocol.TypeRoomJoin, protocol.RoomJoin{Code: c.cfg.Join})
	echo := protocol.MustEncode(protocol.TypeProfile, protocol.Profile{Name: rs.Self.Name, Emoji: rs.Self.Emoji})
	if err := conn.WriteMessage(websocket.TextMessage, join); err != nil {
		return rs, raw, err
	}
	if err := conn.WriteMessage(websocket.TextMessage, echo); err != nil {
		return rs, raw, err
	}
	joined, jraw, err := readRoom(conn, rs.Self.ID)
	if err != nil {
		return rs, raw, err
	}
	if jraw == nil {
		return rs, raw, nil
	}
	return joined, jraw, nil
}

func readRoom(conn *websocket.Conn, selfID string) (protocol.RoomState, json.RawMessage, error) {
	_ = conn.SetReadDeadline(time.Now().Add(handshakeWait))
	for {
		typ, data, err := conn.ReadMessage()
		if err != nil {
			return protocol.RoomState{}, nil, err
		}
		if typ != websocket.TextMessage {
			continue
		}
		env, err := protocol.Decode(data)
		if err != nil {
			continue
		}
		switch env.Type {
		case protocol.TypeRoomState:
			var rs protocol.RoomState
			if err := protocol.UnmarshalData(env, &rs); err != nil {
				return rs, nil, err
			}
			return rs, env.Data, nil
		case protocol.TypePeerUpdated:
			var p protocol.Peer
			if selfID != "" && protocol.UnmarshalData(env, &p) == nil && p.ID == selfID {
				return protocol.RoomState{}, nil, nil
			}
		case protocol.TypeError:
			var e protocol.Error
			_ = protocol.UnmarshalData(env, &e)
			return protocol.RoomState{}, nil, &FatalError{Code: e.Code, Message: e.Message}
		}
	}
}

func (c *Client) track(env protocol.Envelope) error {
	switch env.Type {
	case protocol.TypeRoomState:
		var rs protocol.RoomState
		if protocol.UnmarshalData(env, &rs) == nil {
			c.setRoom(rs)
		}
	case protocol.TypePeerJoined, protocol.TypePeerUpdated:
		var p protocol.Peer
		if protocol.UnmarshalData(env, &p) == nil && p.ID != "" {
			c.pmu.Lock()
			if p.ID == c.self.ID {
				c.self = p
			} else {
				c.peers[p.ID] = p
			}
			c.pmu.Unlock()
		}
	case protocol.TypePeerLeft:
		var p protocol.PeerLeft
		if protocol.UnmarshalData(env, &p) == nil {
			c.pmu.Lock()
			delete(c.peers, p.ID)
			c.pmu.Unlock()
		}
	case protocol.TypeError:
		var e protocol.Error
		if protocol.UnmarshalData(env, &e) == nil &&
			(e.Code == protocol.ErrCodeReplaced || e.Code == protocol.ErrCodeTooManyConns) {
			return &FatalError{Code: e.Code, Message: e.Message}
		}
	}
	return nil
}

func (c *Client) setRoom(rs protocol.RoomState) {
	c.pmu.Lock()
	defer c.pmu.Unlock()
	c.self = rs.Self
	c.peers = make(map[string]protocol.Peer, len(rs.Peers))
	for _, p := range rs.Peers {
		c.peers[p.ID] = p
	}
}

func (c *Client) setConn(conn *websocket.Conn) {
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
}

func (c *Client) emit(ctx context.Context, ev Event) bool {
	select {
	case c.Events <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

func (c *Client) Self() protocol.Peer {
	c.pmu.RLock()
	defer c.pmu.RUnlock()
	return c.self
}

func (c *Client) Peer(id string) (protocol.Peer, bool) {
	c.pmu.RLock()
	defer c.pmu.RUnlock()
	p, ok := c.peers[id]
	return p, ok
}

func (c *Client) Peers() []protocol.Peer {
	c.pmu.RLock()
	out := make([]protocol.Peer, 0, len(c.peers))
	for _, p := range c.peers {
		out = append(out, p)
	}
	c.pmu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (c *Client) Send(msgType string, data any) error {
	b, err := protocol.Encode(msgType, data)
	if err != nil {
		return err
	}
	return c.write(websocket.TextMessage, b)
}

func (c *Client) SendFrame(frame []byte) error {
	return c.write(websocket.BinaryMessage, frame)
}

func (c *Client) write(typ int, b []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return ErrNotConnected
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
	return c.conn.WriteMessage(typ, b)
}

func HumanBytes(n int64) string {
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
