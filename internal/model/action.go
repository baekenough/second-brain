package model

import (
	"time"

	"github.com/google/uuid"
)

// ActionKind is the closed vocabulary of actions.kind (spec §5.4).
type ActionKind string

const (
	KindAwaitingMyReply ActionKind = "awaiting_my_reply"
	KindMyCommitment    ActionKind = "my_commitment"
	KindTheirCommitment ActionKind = "their_commitment"
	KindScheduled       ActionKind = "scheduled"
)

// DetectedBy identifies which pipeline(s) produced an action row.
type DetectedBy string

const (
	DetectedStructural DetectedBy = "structural"
	DetectedLLM        DetectedBy = "llm"
	DetectedBoth       DetectedBy = "both"
)

// ActionState is action_status.state (spec §5.4). Part A only ever writes
// StateOpen, on first observation via ON CONFLICT DO NOTHING (spec §5.5/
// §7.2: re-extraction must never overwrite a user's done/ignored decision).
// StateDone/StateIgnored are written exclusively by Part C (out of scope
// here) in response to explicit user action.
type ActionState string

const (
	StateOpen    ActionState = "open"
	StateDone    ActionState = "done"
	StateIgnored ActionState = "ignored"
)

var validActionKinds = map[ActionKind]bool{
	KindAwaitingMyReply: true,
	KindMyCommitment:    true,
	KindTheirCommitment: true,
	KindScheduled:       true,
}

// IsValidActionKind reports whether k is one of the 4 closed-vocabulary
// kinds. Used by the extraction worker to drop LLM output naming an
// invented kind, rather than persisting a value Part C's UI cannot switch
// on.
func IsValidActionKind(k ActionKind) bool { return validActionKinds[k] }

// Action is a row in the actions table (migration 022).
type Action struct {
	ID                  int64
	IdentityKey         string
	DocumentID          uuid.UUID
	ThreadKey           string
	Kind                ActionKind
	Summary             string
	CounterpartEntityID *int64
	DueAt               *time.Time
	DetectedBy          DetectedBy
	Confidence          float64
	ObservedAt          time.Time
}
