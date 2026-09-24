package store

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Real-database checks for migration 039's citation_verification column
// (issue #268).
//
// A struct-field test (ask_sessions_test.go's TestAskCitationVerification_JSON)
// proves the Go type marshals correctly. It cannot prove that PostgreSQL
// accepts a NULL ::jsonb parameter for this column, that the SELECT column
// order agrees with scanAskSession's Scan target order (this repository has
// already shipped SQL that compiled, passed stub tests, and broke at
// runtime from exactly this kind of drift — a prior incident where
// RETURNING referenced EXCLUDED, only caught by a real-DB test), or that a
// NULL column round-trips to a nil *AskCitationVerification rather than an
// error or a zero-value struct.
//
// Skipped unless TEST_DATABASE_URL is set. It must NEVER point at
// production — run a throwaway pgvector container. Rows are scoped by
// conversation IDs generated per-test and removed in t.Cleanup.
// ---------------------------------------------------------------------------

// askVerificationTestDB connects to TEST_DATABASE_URL or skips the calling
// test, and cleans up every row it inserts via the conversation IDs the
// caller registers with the returned cleanup function.
func askVerificationTestDB(t *testing.T) *AskSessionStore {
	t.Helper()
	pg := srcTestDB(t) // srcTestDB (document_source_types_db_test.go): shared TEST_DATABASE_URL skip/connect/cleanup helper.
	return NewAskSessionStore(pg)
}

// cleanupAskSession removes one conversation's rows regardless of which
// test wrote them — ask_sessions has no source_id column to scope a LIKE
// cleanup against (unlike srcTestDB's document-table convention), so this
// deletes by conversation_id instead.
func cleanupAskSession(t *testing.T, store *AskSessionStore, conversationID uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = store.pg.pool.Exec(context.Background(),
			`DELETE FROM ask_sessions WHERE conversation_id = $1`, conversationID)
	})
}

// TestDB_AskSessionInsert_NilVerification_RoundTripsToNil covers the
// no_evidence / pre-migration-039 case: a nil Verification must insert as
// SQL NULL (not an error, not a marshaled "null" JSON string) and scan back
// as nil, matching scanAskSession's documented contract.
func TestDB_AskSessionInsert_NilVerification_RoundTripsToNil(t *testing.T) {
	store := askVerificationTestDB(t)
	conversationID := uuid.New()
	cleanupAskSession(t, store, conversationID)

	saved, err := store.Insert(context.Background(), AskSession{
		ConversationID: conversationID,
		TurnIndex:      0,
		Question:       "질문",
		Answer:         "",
		FinishReason:   "no_evidence",
		Verification:   nil,
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	got, err := store.Get(context.Background(), saved.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("Get returned nil, want the inserted row")
	}
	if got.Verification != nil {
		t.Errorf("Verification = %+v, want nil", got.Verification)
	}
}

// TestDB_AskSessionInsert_VerificationRoundTrips covers the populated case
// across all three read paths (Get, ListConversationTurns,
// ListRecentConversations) — each has its own SELECT column list
// (ask_sessions.go), so each must be checked independently for the same
// column-order class of bug TestDB_ChunkSearch_ReturnsDocumentTimestamps
// (chunks_timestamps_db_test.go) documents.
func TestDB_AskSessionInsert_VerificationRoundTrips(t *testing.T) {
	store := askVerificationTestDB(t)
	conversationID := uuid.New()
	cleanupAskSession(t, store, conversationID)

	citedID := uuid.New().String()
	verification := &AskCitationVerification{
		CitationStatus:    "invalid",
		CitedIDs:          []string{citedID},
		UnknownIDs:        []string{citedID},
		MalformedLinks:    1,
		InferredCitedIDs:  []string{},
		PromptEvidenceIDs: []string{},
		ClaimSupport:      "not_evaluated",
	}

	saved, err := store.Insert(context.Background(), AskSession{
		ConversationID: conversationID,
		TurnIndex:      0,
		Question:       "질문",
		Answer:         "답변 [근거](/documents/" + citedID + ")",
		FinishReason:   "stop",
		Verification:   verification,
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	check := func(t *testing.T, label string, got *AskCitationVerification) {
		t.Helper()
		if got == nil {
			t.Fatalf("%s: Verification = nil, want a populated report", label)
		}
		if got.CitationStatus != verification.CitationStatus {
			t.Errorf("%s: CitationStatus = %q, want %q", label, got.CitationStatus, verification.CitationStatus)
		}
		if got.MalformedLinks != verification.MalformedLinks {
			t.Errorf("%s: MalformedLinks = %d, want %d", label, got.MalformedLinks, verification.MalformedLinks)
		}
		if len(got.CitedIDs) != 1 || got.CitedIDs[0] != citedID {
			t.Errorf("%s: CitedIDs = %v, want [%s]", label, got.CitedIDs, citedID)
		}
		if len(got.UnknownIDs) != 1 || got.UnknownIDs[0] != citedID {
			t.Errorf("%s: UnknownIDs = %v, want [%s]", label, got.UnknownIDs, citedID)
		}
	}

	t.Run("Get", func(t *testing.T) {
		got, err := store.Get(context.Background(), saved.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got == nil {
			t.Fatal("Get returned nil")
		}
		check(t, "Get", got.Verification)
	})

	t.Run("ListConversationTurns", func(t *testing.T) {
		turns, err := store.ListConversationTurns(context.Background(), conversationID)
		if err != nil {
			t.Fatalf("ListConversationTurns: %v", err)
		}
		if len(turns) != 1 {
			t.Fatalf("len(turns) = %d, want 1", len(turns))
		}
		check(t, "ListConversationTurns", turns[0].Verification)
	})

	t.Run("ListRecentConversations", func(t *testing.T) {
		conversations, err := store.ListRecentConversations(context.Background(), 500)
		if err != nil {
			t.Fatalf("ListRecentConversations: %v", err)
		}
		var found *AskSession
		for i := range conversations {
			if conversations[i].ConversationID == conversationID {
				found = &conversations[i]
				break
			}
		}
		if found == nil {
			t.Fatalf("seeded conversation %s not returned by ListRecentConversations", conversationID)
		}
		check(t, "ListRecentConversations", found.Verification)
	})
}
