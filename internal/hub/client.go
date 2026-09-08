package hub

import (
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/TamerlanK/beam/internal/names"
	"github.com/TamerlanK/beam/internal/protocol"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	writeWait  = 10 * time.Second
	pongWait   = 60 * time.Second
	pingPeriod = 30 * time.Second

	sendBufSize = 64
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
}

var framePool = sync.Pool{
	New: func() any {
		b := make([]byte, protocol.MaxFrameSize)
		return &b
	},
}

type outFrame struct {
	msgType int
	data    []byte

	pool *[]byte

	transferID uuid.UUID
}

type Client struct {
	ID     string
	Name   string
	Emoji  string
	Device string
	IP     string
	Pub    string

	hub  *Hub
	conn *websocket.Conn
	send chan outFrame
	room *Room
	log  *slog.Logger
}

func (c *Client) Peer() protocol.Peer {
	return protocol.Peer{ID: c.ID, Name: c.Name, Emoji: c.Emoji, Device: c.Device, Pub: c.Pub}
}

func ServeWS(h *Hub, w http.ResponseWriter, r *http.Request, trustProxy bool) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.log.Warn("websocket upgrade failed", "err", err, "remote", r.RemoteAddr)
		return
	}
	id, name, emoji := identity(r)
	c := &Client{
		ID:     id,
		Name:   name,
		Emoji:  emoji,
		Pub:    publicKey(r.URL.Query().Get("pub")),
		Device: deviceType(r.UserAgent()),
		IP:     clientIP(r, trustProxy),
		hub:    h,
		conn:   conn,
		send:   make(chan outFrame, sendBufSize),
	}
	c.log = h.log.With("client", c.ID, "name", c.Name)

	select {
	case h.register <- c:
	case <-h.done:
		_ = conn.Close()
		return
	}
	go c.writePump()
	c.readPump()
}

func identity(r *http.Request) (id, name, emoji string) {
	q := r.URL.Query()
	id = uuid.NewString()
	if u, err := uuid.Parse(q.Get("id")); err == nil && u.Version() == 4 {
		id = u.String()
	}
	name, emoji = names.CleanName(q.Get("name")), names.CleanEmoji(q.Get("emoji"))
	if name == "" || emoji == "" {
		rn, re := names.Random()
		if name == "" {
			name = rn
		}
		if emoji == "" {
			emoji = re
		}
	}
	return id, name, emoji
}

func publicKey(s string) string {
	if len(s) > protocol.MaxKeyChars {
		return ""
	}
	if b, err := base64.StdEncoding.DecodeString(s); err != nil || len(b) != 65 || b[0] != 4 {
		return ""
	}
	return s
}

func (c *Client) readPump() {
	defer func() {
		c.hub.drop(c)
		_ = c.conn.Close()
	}()
	c.conn.SetReadLimit(protocol.MaxControlMessage)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(pongWait))
	})
	for {
		msgType, r, err := c.conn.NextReader()
		if err != nil {
			return
		}
		switch msgType {
		case websocket.TextMessage:
			data, err := io.ReadAll(r)
			if err != nil {
				return
			}
			if !c.hub.deliver(inbound{from: c, text: data}) {
				return
			}
		case websocket.BinaryMessage:
			bp := framePool.Get().(*[]byte)
			n, err := readFrame(r, *bp)
			if err != nil {
				framePool.Put(bp)
				c.log.Warn("dropping client: bad binary frame", "err", err)
				return
			}
			if !c.hub.deliver(inbound{from: c, frame: (*bp)[:n], pool: bp}) {
				framePool.Put(bp)
				return
			}
		}
	}
}

func readFrame(r io.Reader, buf []byte) (int, error) {
	n, err := io.ReadFull(r, buf)
	switch err {
	case nil:

		var one [1]byte
		if _, err := io.ReadFull(r, one[:]); err != io.EOF {
			return n, errors.New("binary frame exceeds max frame size")
		}
		return n, nil
	case io.EOF, io.ErrUnexpectedEOF:
		return n, nil
	default:
		return n, err
	}
}

func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		_ = c.conn.Close()
	}()
	for {
		select {
		case f, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseGoingAway, ""))
				return
			}
			err := c.conn.WriteMessage(f.msgType, f.data)
			payload := len(f.data) - protocol.FrameOverhead
			if f.pool != nil {
				framePool.Put(f.pool)
			}
			if err != nil {
				return
			}
			if f.transferID != uuid.Nil {
				select {
				case c.hub.written <- written{to: c, id: f.transferID, n: payload}:
				case <-c.hub.done:
					return
				}
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			if first = strings.TrimSpace(first); first != "" {
				return first
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func deviceType(ua string) string {
	ua = strings.ToLower(ua)
	mobile := strings.Contains(ua, "mobi")
	switch {
	case strings.Contains(ua, "ipad"), strings.Contains(ua, "tablet"),
		strings.Contains(ua, "android") && !mobile:
		return "tablet"
	case mobile, strings.Contains(ua, "iphone"), strings.Contains(ua, "android"):
		return "phone"
	default:
		return "laptop"
	}
}
