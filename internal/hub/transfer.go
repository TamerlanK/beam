package hub

import (
	"fmt"
	"time"

	"github.com/TamerlanK/beam/internal/protocol"
	"github.com/google/uuid"
)

type TransferState int

const (
	StateOffered TransferState = iota
	StateAccepted
	StateActive
	StateCompleted
	StateDeclined
	StateCanceled
	StateFailed
)

var stateNames = [...]string{"offered", "accepted", "active", "completed", "declined", "canceled", "failed"}

func (s TransferState) String() string {
	if int(s) < len(stateNames) {
		return stateNames[s]
	}
	return fmt.Sprintf("state(%d)", int(s))
}

func (s TransferState) Terminal() bool {
	switch s {
	case StateCompleted, StateDeclined, StateCanceled, StateFailed:
		return true
	}
	return false
}

var legal = map[TransferState]map[TransferState]bool{
	StateOffered:  {StateAccepted: true, StateDeclined: true, StateCanceled: true, StateFailed: true},
	StateAccepted: {StateActive: true, StateCanceled: true, StateFailed: true},
	StateActive:   {StateCompleted: true, StateCanceled: true, StateFailed: true},
}

type Transfer struct {
	ID     uuid.UUID
	FromID string
	ToID   string
	Name   string
	Size   int64
	Mime   string
	Key    string

	Wire int64

	State TransferState

	Relayed int64
	Written int64

	Deadline time.Time
}

func newTransfer(id uuid.UUID, fromID, toID, name string, size int64, mime string, now time.Time) *Transfer {
	return &Transfer{
		ID:       id,
		FromID:   fromID,
		ToID:     toID,
		Name:     name,
		Size:     size,
		Mime:     mime,
		Wire:     size,
		State:    StateOffered,
		Deadline: now.Add(protocol.OfferTimeoutSec * time.Second),
	}
}

func (t *Transfer) transition(to TransferState) error {
	if !legal[t.State][to] {
		return fmt.Errorf("illegal transition %s → %s", t.State, to)
	}
	t.State = to
	return nil
}

func (t *Transfer) Answer(actorID string, accept bool) error {
	if actorID != t.ToID {
		return fmt.Errorf("answer from %q, but transfer is addressed to %q", actorID, t.ToID)
	}
	if accept {
		return t.transition(StateAccepted)
	}
	return t.transition(StateDeclined)
}

func (t *Transfer) Encrypt() {
	t.Wire = protocol.WireSize(t.Size)
}

func (t *Transfer) Cancel(actorID string) error {
	if actorID != t.FromID && actorID != t.ToID {
		return fmt.Errorf("cancel from %q, not a party to the transfer", actorID)
	}
	return t.transition(StateCanceled)
}

func (t *Transfer) Chunk(actorID string, n int) error {
	if actorID != t.FromID {
		return fmt.Errorf("chunk from %q, but sender is %q", actorID, t.FromID)
	}
	if t.State == StateAccepted {
		if err := t.transition(StateActive); err != nil {
			return err
		}
	}
	if t.State != StateActive {
		return fmt.Errorf("chunk while %s", t.State)
	}
	if t.Relayed+int64(n) > t.Wire {
		return fmt.Errorf("received %d bytes, more than declared size %d", t.Relayed+int64(n), t.Wire)
	}
	t.Relayed += int64(n)
	return nil
}

func (t *Transfer) NoteWritten(n int) (completed bool, err error) {
	t.Written += int64(n)
	if t.State == StateActive && t.Written >= t.Wire {
		return true, t.transition(StateCompleted)
	}
	return false, nil
}

func (t *Transfer) Fail() bool {
	if t.State.Terminal() {
		return false
	}
	t.State = StateFailed
	return true
}
