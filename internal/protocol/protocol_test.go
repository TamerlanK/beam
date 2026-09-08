package protocol

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestRoundTrip(t *testing.T) {
	peer := &Peer{ID: "p1", Name: "Purple Falcon", Emoji: "🦅", Device: "laptop"}
	cases := []struct {
		msgType string
		payload any
	}{
		{TypeRoomState, RoomState{Self: *peer, Peers: []Peer{{ID: "p2", Name: "Amber Otter", Emoji: "🦦", Device: "phone"}}}},
		{TypePeerJoined, *peer},
		{TypePeerLeft, PeerLeft{ID: "p2"}},
		{TypeRoomCreate, nil},
		{TypeRoomCreated, RoomCreated{Code: "ABCD", ExpiresIn: 600}},
		{TypeRoomJoin, RoomJoin{Code: "WXYZ"}},
		{TypeTransferOffer, TransferOffer{ID: uuid.NewString(), To: "p2", Name: "photo.jpg", Size: 12345, Mime: "image/jpeg"}},
		{TypeTransferOffer, TransferOffer{ID: uuid.NewString(), From: peer, Name: "a.bin", Size: 1}},
		{TypeTransferOffer, TransferOffer{ID: uuid.NewString(), To: "p2", Name: "a.jpg", Path: "photos/2024", Size: 1, Batch: &Batch{ID: uuid.NewString(), Files: 2, Bytes: 3}}},
		{TypeDropWaiting, DropWaiting{ID: uuid.NewString(), From: peer, Name: "a.jpg", Path: "photos", Size: 1, ExpiresIn: 5}},
		{TypeTransferAnswer, TransferAnswer{ID: uuid.NewString(), Accept: true}},
		{TypeTransferCancel, TransferCancel{ID: uuid.NewString(), Reason: "changed my mind"}},
		{TypeTransferComplete, TransferComplete{ID: uuid.NewString(), Bytes: 987654321}},
		{TypeTransferFailed, TransferFailed{ID: uuid.NewString(), Reason: "peer-disconnected"}},
		{TypeFlowCredit, FlowCredit{ID: uuid.NewString(), N: 1}},
		{TypeSnippet, Snippet{To: "p2", Text: "https://example.com"}},
		{TypeError, Error{Code: ErrCodeBadCode, Message: "no such room"}},
	}
	for _, tc := range cases {
		t.Run(tc.msgType, func(t *testing.T) {
			raw, err := Encode(tc.msgType, tc.payload)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			env, err := Decode(raw)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if env.V != Version || env.Type != tc.msgType {
				t.Fatalf("envelope = %+v, want v=%d type=%s", env, Version, tc.msgType)
			}
			if tc.payload == nil {
				return
			}
			got := reflect.New(reflect.TypeOf(tc.payload))
			if err := json.Unmarshal(env.Data, got.Interface()); err != nil {
				t.Fatalf("payload unmarshal: %v", err)
			}
			if !reflect.DeepEqual(got.Elem().Interface(), tc.payload) {
				t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got.Elem().Interface(), tc.payload)
			}
		})
	}
}

func TestDecodeRejects(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		err  error
	}{
		{"garbage", "not json", ErrBadJSON},
		{"empty", "", ErrBadJSON},
		{"wrong version", `{"v":2,"type":"error"}`, ErrBadVersion},
		{"zero version", `{"type":"error"}`, ErrBadVersion},
		{"no type", `{"v":1}`, ErrNoType},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Decode([]byte(tc.raw)); err != tc.err {
				t.Fatalf("Decode(%q) err = %v, want %v", tc.raw, err, tc.err)
			}
		})
	}
}

func TestSplitPath(t *testing.T) {
	good := map[string][]string{
		"":                 nil,
		"photos":           {"photos"},
		"photos/2024":      {"photos", "2024"},
		`photos\2024\sub`:  {"photos", "2024", "sub"},
		"a b/c.d/..e":      {"a b", "c.d", "..e"},
		"..." + "/" + "x.": {"...", "x."},
	}
	for in, want := range good {
		got, ok := SplitPath(in)
		if !ok || !reflect.DeepEqual(got, want) {
			t.Errorf("SplitPath(%q) = %v, %v; want %v, true", in, got, ok, want)
		}
	}
	bad := []string{"../x", "a/../b", "a/./b", "/abs", "a//b", "a/", ".", "..", strings.Repeat("a", MaxPathBytes+1), "a/" + strings.Repeat("b", MaxFilenameBytes+1)}
	for _, in := range bad {
		if _, ok := SplitPath(in); ok {
			t.Errorf("SplitPath(%q) accepted", in)
		}
	}
}

func TestValidBatch(t *testing.T) {
	id := uuid.NewString()
	if !ValidBatch(nil) || !ValidBatch(&Batch{ID: id, Files: 1, Bytes: 1}) {
		t.Fatal("valid batch rejected")
	}
	for _, b := range []*Batch{
		{ID: "x", Files: 1, Bytes: 1},
		{ID: uuid.Nil.String(), Files: 1, Bytes: 1},
		{ID: id, Files: 0, Bytes: 1},
		{ID: id, Files: 1, Bytes: 0},
		{ID: id, Files: MaxBatchFiles + 1, Bytes: 1},
		{ID: id, Files: 1, Bytes: -1},
	} {
		if ValidBatch(b) {
			t.Errorf("ValidBatch(%+v) accepted", *b)
		}
	}
}

func TestFrameRoundTrip(t *testing.T) {
	id := uuid.New()
	chunk := bytes.Repeat([]byte{0xAB}, ChunkSize)
	buf := make([]byte, MaxFrameSize)
	frame := EncodeFrame(buf, id, chunk)
	if len(frame) != FrameOverhead+ChunkSize {
		t.Fatalf("frame len = %d, want %d", len(frame), FrameOverhead+ChunkSize)
	}
	gotID, gotChunk, err := SplitFrame(frame)
	if err != nil {
		t.Fatalf("SplitFrame: %v", err)
	}
	if gotID != id || !bytes.Equal(gotChunk, chunk) {
		t.Fatal("frame round trip mismatch")
	}
}

func TestSplitFrameRejects(t *testing.T) {
	for _, n := range []int{0, 1, FrameOverhead, MaxFrameSize + 1} {
		if _, _, err := SplitFrame(make([]byte, n)); err == nil {
			t.Errorf("SplitFrame accepted %d-byte frame", n)
		}
	}
}
