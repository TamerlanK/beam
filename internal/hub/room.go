package hub

import (
	"crypto/rand"
	"time"

	"github.com/TamerlanK/beam/internal/protocol"
)

type Room struct {
	key     string
	clients map[string]*Client

	code        string
	codeExpires time.Time
}

func newRoom(key string) *Room {
	return &Room{key: key, clients: map[string]*Client{}}
}

func (r *Room) peersExcept(except *Client) []protocol.Peer {
	peers := make([]protocol.Peer, 0, len(r.clients))
	for _, c := range r.clients {
		if c != except {
			peers = append(peers, c.Peer())
		}
	}
	return peers
}

func newRoomCode() string {
	var b [protocol.RoomCodeLen]byte
	if _, err := rand.Read(b[:]); err != nil {

		panic("crypto/rand: " + err.Error())
	}
	for i := range b {
		b[i] = protocol.RoomCodeAlphabet[b[i]&31]
	}
	return string(b[:])
}
