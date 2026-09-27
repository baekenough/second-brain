package search

import (
	"context"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
)

func TestServiceKakaoExplicitOnlyAcrossLanes(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		doc := &model.SearchResult{Document: model.Document{ID: uuid.New(), SourceType: model.SourceKakao, Content: "합성 카카오 원문"}, Score: 1}
		docs := &recordingDocSearcher{results: []*model.SearchResult{doc}}
		chunk := store.ChunkSearchResult{Chunk: store.Chunk{ID: 1, DocumentID: doc.ID, Content: doc.Content}, Score: 1, Rank: 1, DocumentSource: string(model.SourceKakao), DocumentStatus: "active"}
		chunks := &mockChunkSearcher{vectorResults: []store.ChunkSearchResult{chunk}, ftsResults: []store.ChunkSearchResult{chunk}}
		q := model.SearchQuery{Query: "합성", Limit: 10}
		if explicit {
			q.SourceTypes = []model.SourceType{model.SourceKakao}
		}
		results, err := NewService(docs, stubEnabledEmbedder{}).WithChunkStore(chunks).Search(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		if explicit != (len(results) > 0) {
			t.Fatalf("explicit=%v results=%d", explicit, len(results))
		}
		if containsSourceType(docs.gotQuery.ExcludeSourceTypes, model.SourceKakao) == explicit {
			t.Fatalf("wrong downstream eligibility: %+v", docs.gotQuery.ExcludeSourceTypes)
		}
	}
}
