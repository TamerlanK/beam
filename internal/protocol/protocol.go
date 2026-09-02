package protocol

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

const Version = 1

const (
	ChunkSize = 64 * 1024

	FrameOverhead = 16

	MaxFrameSize = FrameOverhead + ChunkSize

	CreditWindow = 16

	MaxControlMessage = 1 << 20

	MaxFilenameBytes = 255

	MaxDeclaredSize = 50 << 30

	MaxSnippetBytes = 8 * 1024

	OfferTimeoutSec = 30

	RoomCodeLen = 4

	RoomCodeTTLSec = 600

	MaxNameRunes = 32

	MaxEmojiBytes = 32
)

const RoomCodeAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

const (
	TypeRoomState        = "room-state"
	TypePeerJoined       = "peer-joined"
	TypePeerLeft         = "peer-left"
	TypePeerUpdated      = "peer-updated"
	TypeProfile          = "profile"
	TypeRoomCreate       = "room-create"
	TypeRoomCreated      = "room-created"
	TypeRoomJoin         = "room-join"
	TypeTransferOffer    = "transfer-offer"
	TypeTransferAnswer   = "transfer-answer"
	TypeTransferCancel   = "transfer-cancel"
	TypeTransferComplete = "transfer-complete"
	TypeTransferFailed   = "transfer-failed"
	TypeFlowCredit       = "flow-credit"
	TypeSnippet          = "snippet"
	TypeError            = "error"
)

type Envelope struct {
	V    int             `json:"v"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

var (
	ErrBadJSON    = errors.New("protocol: malformed JSON")
	ErrBadVersion = errors.New("protocol: unsupported version")
	ErrNoType     = errors.New("protocol: missing message type")
)

func Encode(msgType string, data any) ([]byte, error) {
	var raw json.RawMessage
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	return json.Marshal(Envelope{V: Version, Type: msgType, Data: raw})
}

func MustEncode(msgType string, data any) []byte {
	b, err := Encode(msgType, data)
	if err != nil {
		panic(fmt.Sprintf("protocol: encode %s: %v", msgType, err))
	}
	return b
}

func Decode(raw []byte) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return Envelope{}, ErrBadJSON
	}
	if env.V != Version {
		return Envelope{}, ErrBadVersion
	}
	if env.Type == "" {
		return Envelope{}, ErrNoType
	}
	return env, nil
}

func UnmarshalData(env Envelope, v any) error {
	if len(env.Data) == 0 {
		return errors.New("protocol: missing payload")
	}
	return json.Unmarshal(env.Data, v)
}

type Peer struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Emoji  string `json:"emoji"`
	Device string `json:"device"`
}

type RoomState struct {
	Self  Peer   `json:"self"`
	Peers []Peer `json:"peers"`
}

type Profile struct {
	Name  string `json:"name"`
	Emoji string `json:"emoji"`
}

type PeerLeft struct {
	ID string `json:"id"`
}

type RoomCreated struct {
	Code      string `json:"code"`
	ExpiresIn int    `json:"expiresIn"`
}

type RoomJoin struct {
	Code string `json:"code"`
}

type TransferOffer struct {
	ID   string `json:"id"`
	To   string `json:"to,omitempty"`
	From *Peer  `json:"from,omitempty"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	Mime string `json:"mime,omitempty"`
}

type TransferAnswer struct {
	ID     string `json:"id"`
	Accept bool   `json:"accept"`
}

type TransferCancel struct {
	ID     string `json:"id"`
	Reason string `json:"reason,omitempty"`
}

type TransferComplete struct {
	ID    string `json:"id"`
	Bytes int64  `json:"bytes"`
}

type TransferFailed struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

type FlowCredit struct {
	ID string `json:"id"`
	N  int    `json:"n"`
}

type Snippet struct {
	To   string `json:"to,omitempty"`
	From *Peer  `json:"from,omitempty"`
	Text string `json:"text"`
}

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

const (
	ErrCodeBadMessage  = "bad-message"
	ErrCodeBadCode     = "bad-code"
	ErrCodeRateLimited = "rate-limited"
	ErrCodeUnknownPeer = "unknown-peer"
	ErrCodeBadTransfer = "bad-transfer"
)

func EncodeFrame(dst []byte, id uuid.UUID, chunk []byte) []byte {
	copy(dst[:FrameOverhead], id[:])
	copy(dst[FrameOverhead:], chunk)
	return dst[:FrameOverhead+len(chunk)]
}

func SplitFrame(frame []byte) (uuid.UUID, []byte, error) {
	if len(frame) <= FrameOverhead || len(frame) > MaxFrameSize {
		return uuid.UUID{}, nil, fmt.Errorf("protocol: bad frame length %d", len(frame))
	}
	var id uuid.UUID
	copy(id[:], frame[:FrameOverhead])
	return id, frame[FrameOverhead:], nil
}
