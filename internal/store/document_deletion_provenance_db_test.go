package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

func TestDeletedDocumentReingest(t *testing.T) {
	pg := callProtectTestDB(t)
	ds := NewDocumentStore(pg)
	ctx := context.Background()
	for _, actor := range []string{"", "user", "policy", "filesystem", "system"} {
		for _, method := range []string{"upsert", "tracked", "transcript", "chunks"} {
			t.Run(actor+"/"+method, func(t *testing.T) {
				doc := callLogDoc(callProtectTestPrefix+uuid.NewString(), uniqContact("deleted"), "pending")
				if err := ds.Upsert(ctx, doc); err != nil {
					t.Fatal(err)
				}
				oldContent := doc.Content
				_, err := pg.pool.Exec(ctx, `UPDATE documents SET status='deleted', deleted_at=now(), deleted_by=NULLIF($2,'') WHERE id=$1`, doc.ID, actor)
				if err != nil {
					t.Fatal(err)
				}
				doc.Content = "replacement " + uuid.NewString()
				doc.Metadata = map[string]any{"transcription": "done"}
				changed, built := false, false
				switch method {
				case "upsert":
					err = ds.Upsert(ctx, doc)
				case "tracked":
					changed, err = ds.UpsertTracked(ctx, doc)
				case "transcript":
					changed, err = ds.AttachTranscript(ctx, doc)
				case "chunks":
					changed, err = ds.UpsertTrackedWithChunks(ctx, doc, func(*model.Document) []Chunk { built = true; return nil })
				}
				blocked := actor != "filesystem" && actor != "system"
				if blocked {
					if !errors.Is(err, ErrDocumentDeleted) || changed || built {
						t.Fatalf("err=%v changed=%v built=%v", err, changed, built)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				row := readCallRow(t, pg, doc.SourceID)
				if blocked && (row.status != "deleted" || row.content != oldContent) {
					t.Fatalf("deleted content overwritten: %+v", row)
				}
				if !blocked && (row.status != "active" || row.content != doc.Content) {
					t.Fatalf("reappearance failed: %+v", row)
				}
			})
		}
	}
}

func TestDeletedNoteCannotProduceInsights(t *testing.T) {
	pg := callProtectTestDB(t)
	ds := NewDocumentStore(pg)
	ctx := context.Background()
	note := &model.Document{SourceType: model.SourceNote, SourceID: callProtectTestPrefix + uuid.NewString(), Content: "source", Metadata: map[string]any{"enrichment_status": "pending"}, CollectedAt: time.Now()}
	if err := ds.Upsert(ctx, note); err != nil {
		t.Fatal(err)
	}
	makeInsight := func() *model.Document {
		return &model.Document{SourceType: model.SourceInsight, SourceID: callProtectTestPrefix + uuid.NewString(), Content: "derived", Metadata: map[string]any{"provenance": map[string]any{"source_note_id": note.ID.String()}}, CollectedAt: time.Now()}
	}
	existing := makeInsight()
	if err := ds.Upsert(ctx, existing); err != nil {
		t.Fatal(err)
	}
	if err := ds.SoftDeleteByID(ctx, note.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := ds.SoftDeleteInsightsByNoteID(ctx, note.ID); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if err := ds.Upsert(ctx, makeInsight()); err == nil {
		t.Fatal("new insight from deleted note accepted")
	}
	if err := ds.Upsert(ctx, existing); err == nil {
		t.Fatal("deleted insight revived")
	}
	if err := ds.Upsert(ctx, note); !errors.Is(err, ErrDocumentDeleted) {
		t.Fatalf("note revive err=%v", err)
	}
	pending, err := ds.ListPendingNotes(ctx, 10000)
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range pending {
		if doc.ID == note.ID {
			t.Fatal("deleted note selected")
		}
	}
}
