package hub

import (
	"strings"
	"testing"
	"time"

	"github.com/TamerlanK/beam/internal/protocol"
	"github.com/google/uuid"
)

func testTransfer() *Transfer {
	return newTransfer(uuid.New(), "sender", "receiver", "file.bin", 100, "", time.Now())
}

func TestTransferHappyPath(t *testing.T) {
	tr := testTransfer()
	if tr.State != StateOffered {
		t.Fatalf("new transfer state = %s", tr.State)
	}
	if err := tr.Answer("receiver", true); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if tr.State != StateAccepted {
		t.Fatalf("state = %s, want accepted", tr.State)
	}
	if err := tr.Chunk("sender", 60); err != nil {
		t.Fatalf("first chunk: %v", err)
	}
	if tr.State != StateActive {
		t.Fatalf("state = %s, want active", tr.State)
	}
	if err := tr.Chunk("sender", 40); err != nil {
		t.Fatalf("second chunk: %v", err)
	}
	if done, err := tr.NoteWritten(60); done || err != nil {
		t.Fatalf("NoteWritten(60) = %v, %v", done, err)
	}
	done, err := tr.NoteWritten(40)
	if err != nil || !done {
		t.Fatalf("NoteWritten(40) = %v, %v; want completed", done, err)
	}
	if tr.State != StateCompleted || !tr.State.Terminal() {
		t.Fatalf("state = %s, want completed (terminal)", tr.State)
	}
}

func TestTransferDecline(t *testing.T) {
	tr := testTransfer()
	if err := tr.Answer("receiver", false); err != nil {
		t.Fatalf("decline: %v", err)
	}
	if tr.State != StateDeclined {
		t.Fatalf("state = %s, want declined", tr.State)
	}
}

func TestTransferMaliciousMoves(t *testing.T) {
	cases := []struct {
		name string
		run  func(tr *Transfer) error
	}{
		{"answer from sender", func(tr *Transfer) error { return tr.Answer("sender", true) }},
		{"answer from stranger", func(tr *Transfer) error { return tr.Answer("mallory", true) }},
		{"double accept", func(tr *Transfer) error {
			must(t, tr.Answer("receiver", true))
			return tr.Answer("receiver", true)
		}},
		{"decline after accept", func(tr *Transfer) error {
			must(t, tr.Answer("receiver", true))
			return tr.Answer("receiver", false)
		}},
		{"cancel from stranger", func(tr *Transfer) error { return tr.Cancel("mallory") }},
		{"cancel after complete", func(tr *Transfer) error {
			must(t, tr.Answer("receiver", true))
			must(t, tr.Chunk("sender", 100))
			if _, err := tr.NoteWritten(100); err != nil {
				t.Fatal(err)
			}
			return tr.Cancel("sender")
		}},
		{"chunk before accept", func(tr *Transfer) error { return tr.Chunk("sender", 10) }},
		{"chunk from receiver", func(tr *Transfer) error {
			must(t, tr.Answer("receiver", true))
			return tr.Chunk("receiver", 10)
		}},
		{"chunk past declared size", func(tr *Transfer) error {
			must(t, tr.Answer("receiver", true))
			must(t, tr.Chunk("sender", 100))
			return tr.Chunk("sender", 1)
		}},
		{"chunk after decline", func(tr *Transfer) error {
			must(t, tr.Answer("receiver", false))
			return tr.Chunk("sender", 10)
		}},
		{"answer after cancel", func(tr *Transfer) error {
			must(t, tr.Cancel("sender"))
			return tr.Answer("receiver", true)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := testTransfer()
			if err := tc.run(tr); err == nil {
				t.Fatalf("expected an error, transfer ended in state %s", tr.State)
			}
		})
	}
}

func TestTransferCancelBothSides(t *testing.T) {
	for _, actor := range []string{"sender", "receiver"} {
		tr := testTransfer()
		if err := tr.Cancel(actor); err != nil {
			t.Fatalf("cancel by %s: %v", actor, err)
		}
		if tr.State != StateCanceled {
			t.Fatalf("state = %s, want canceled", tr.State)
		}
	}
}

func TestTransferFail(t *testing.T) {
	tr := testTransfer()
	if !tr.Fail() {
		t.Fatal("Fail on live transfer returned false")
	}
	if tr.Fail() {
		t.Fatal("Fail on terminal transfer returned true")
	}
	if tr.State != StateFailed {
		t.Fatalf("state = %s", tr.State)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestRoomCodeAlphabet(t *testing.T) {
	if len(protocol.RoomCodeAlphabet) != 32 {
		t.Fatalf("alphabet has %d chars, want 32 (needed for bias-free masking)", len(protocol.RoomCodeAlphabet))
	}
	for _, banned := range "0O1I" {
		if strings.ContainsRune(protocol.RoomCodeAlphabet, banned) {
			t.Errorf("ambiguous character %q in alphabet", banned)
		}
	}
	for range 200 {
		code := newRoomCode()
		if len(code) != protocol.RoomCodeLen {
			t.Fatalf("code %q has wrong length", code)
		}
		for _, r := range code {
			if !strings.ContainsRune(protocol.RoomCodeAlphabet, r) {
				t.Fatalf("code %q contains %q, outside the alphabet", code, r)
			}
		}
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := []struct{ in, want string }{
		{"photo.jpg", "photo.jpg"},
		{"../../etc/passwd", "passwd"},
		{`C:\Windows\evil.exe`, "evil.exe"},
		{"a/b/c.txt", "c.txt"},
		{"bad\x00name\x1f.txt", "badname.txt"},
		{"..", ""},
		{".", ""},
		{"", ""},
		{strings.Repeat("x", 300), strings.Repeat("x", 255)},
		{strings.Repeat("é", 200), strings.Repeat("é", 127)},
	}
	for _, tc := range cases {
		if got := sanitizeFilename(tc.in); got != tc.want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestTransferEncryptedWire(t *testing.T) {
	size := int64(2*protocol.ChunkSize + 1)
	tr := newTransfer(uuid.New(), "sender", "receiver", "file.bin", size, "", time.Now())
	if err := tr.Answer("receiver", true); err != nil {
		t.Fatal(err)
	}
	tr.Encrypt()
	if want := size + 3*protocol.TagBytes; tr.Wire != want {
		t.Fatalf("Wire = %d, want %d", tr.Wire, want)
	}
	for _, n := range []int{protocol.ChunkSize + protocol.TagBytes, protocol.ChunkSize + protocol.TagBytes, 1 + protocol.TagBytes} {
		if err := tr.Chunk("sender", n); err != nil {
			t.Fatalf("chunk %d: %v", n, err)
		}
		if done, err := tr.NoteWritten(n); err != nil {
			t.Fatal(err)
		} else if done != (tr.Written == tr.Wire) {
			t.Fatalf("done = %v at %d/%d", done, tr.Written, tr.Wire)
		}
	}
	if tr.State != StateCompleted {
		t.Fatalf("state = %s", tr.State)
	}
	if err := tr.Chunk("sender", 1); err == nil {
		t.Fatal("chunk beyond wire size accepted")
	}
}

func TestTransferResumeBaseline(t *testing.T) {
	size := int64(3*protocol.ChunkSize + 1)
	offset := int64(2 * protocol.ChunkSize)

	plain := newTransfer(uuid.New(), "sender", "receiver", "file.bin", size, "", time.Now())
	must(t, plain.Answer("receiver", true))
	plain.Start(offset)
	if plain.Relayed != offset || plain.Written != offset || plain.Offset != offset {
		t.Fatalf("plain baseline = relayed %d written %d", plain.Relayed, plain.Written)
	}
	must(t, plain.Chunk("sender", protocol.ChunkSize))
	must(t, plain.Chunk("sender", 1))
	if err := plain.Chunk("sender", 1); err == nil {
		t.Fatal("chunk beyond the remainder accepted")
	}
	if done, err := plain.NoteWritten(protocol.ChunkSize); done || err != nil {
		t.Fatalf("plain resume completed early: done=%v err=%v", done, err)
	}
	if done, _ := plain.NoteWritten(1); !done || plain.State != StateCompleted {
		t.Fatalf("plain resume did not complete: %s", plain.State)
	}

	enc := newTransfer(uuid.New(), "sender", "receiver", "file.bin", size, "", time.Now())
	must(t, enc.Answer("receiver", true))
	enc.Encrypt()
	enc.Start(offset)
	if want := offset + 2*protocol.TagBytes; enc.Relayed != want || enc.Written != want {
		t.Fatalf("encrypted baseline = relayed %d written %d, want %d", enc.Relayed, enc.Written, want)
	}
	must(t, enc.Chunk("sender", protocol.ChunkSize+protocol.TagBytes))
	must(t, enc.Chunk("sender", 1+protocol.TagBytes))
	if done, err := enc.NoteWritten(protocol.ChunkSize + protocol.TagBytes); done || err != nil {
		t.Fatalf("encrypted resume completed early: done=%v err=%v", done, err)
	}
	if done, _ := enc.NoteWritten(1 + protocol.TagBytes); !done || enc.State != StateCompleted {
		t.Fatalf("encrypted resume did not complete: %s", enc.State)
	}
}
