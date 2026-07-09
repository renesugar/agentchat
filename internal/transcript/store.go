package transcript

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/example/agentchat/internal/adapter"
)

// ErrNotFound is returned when a conversation or turn does not exist.
var ErrNotFound = errors.New("transcript: not found")

// Store persists conversations, turns, and per-turn event streams.
//
// Contract:
//   - BeginTurn creates a Turn with Status == TurnRunning and assigns
//     ID, Seq (1-based, dense per conversation), and StartedAt.
//   - AppendEvent may only be called between BeginTurn and FinishTurn.
//   - FinishTurn sets EndedAt, Result/Error, and the final status
//     (TurnDone when runErr == nil, else TurnFailed), and bumps the
//     conversation's UpdatedAt.
//   - Events returns the events of a turn in append order.
type Store interface {
	CreateConversation(ctx context.Context, nc NewConversation) (*Conversation, error)
	GetConversation(ctx context.Context, id string) (*Conversation, error)
	ListConversations(ctx context.Context) ([]*Conversation, error)

	BeginTurn(ctx context.Context, convID string, nt NewTurn) (*Turn, error)
	AppendEvent(ctx context.Context, convID, turnID string, e adapter.Event) error
	FinishTurn(ctx context.Context, convID, turnID string, res *adapter.Result, runErr error) (*Turn, error)

	ListTurns(ctx context.Context, convID string) ([]*Turn, error)
	Events(ctx context.Context, convID, turnID string) ([]adapter.Event, error)
}

// newID returns a sortable, unique identifier: UTC timestamp + random hex.
func newID(now time.Time) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is unrecoverable for our purposes.
		panic(fmt.Sprintf("transcript: rand: %v", err))
	}
	return now.UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b[:])
}
