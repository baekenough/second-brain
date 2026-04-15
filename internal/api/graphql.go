package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/language/ast"
	gqlhandler "github.com/graphql-go/handler"
	"github.com/baekenough/second-brain/internal/curation"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
)

// jsonScalar is a custom scalar that serializes/deserializes arbitrary JSON
// (map[string]any, []any, or any JSON-compatible value). It is used for
// Document.metadata and baselineStats which contain dynamically shaped data.
var jsonScalar = graphql.NewScalar(graphql.ScalarConfig{
	Name:        "JSON",
	Description: "Arbitrary JSON value (object, array, scalar).",
	Serialize: func(value interface{}) interface{} {
		return value
	},
	ParseValue: func(value interface{}) interface{} {
		return value
	},
	ParseLiteral: func(valueAST ast.Value) interface{} {
		// Accept string literals containing JSON or inline object/array literals.
		switch v := valueAST.(type) {
		case *ast.StringValue:
			var out interface{}
			if err := json.Unmarshal([]byte(v.Value), &out); err == nil {
				return out
			}
			return v.Value
		default:
			return nil
		}
	},
})

// documentType is the GraphQL object type that mirrors model.Document.
var documentType = graphql.NewObject(graphql.ObjectConfig{
	Name:        "Document",
	Description: "A document collected from an external source.",
	Fields: graphql.Fields{
		"id":           {Type: graphql.String, Description: "UUID of the document."},
		"source_type":  {Type: graphql.String, Description: "Origin source type (e.g. slack, github)."},
		"source_id":    {Type: graphql.String, Description: "Source-native identifier."},
		"title":        {Type: graphql.String, Description: "Human-readable title."},
		"content":      {Type: graphql.String, Description: "Full text content."},
		"metadata":     {Type: jsonScalar, Description: "Arbitrary key/value metadata."},
		"status":       {Type: graphql.String, Description: "Document status: active | deleted | moved."},
		"collected_at": {Type: graphql.String, Description: "RFC3339 timestamp of collection."},
		"created_at":   {Type: graphql.String, Description: "RFC3339 timestamp of database insertion."},
		"updated_at":   {Type: graphql.String, Description: "RFC3339 timestamp of last update."},
	},
})

// searchResultType wraps documentType with relevance scoring fields.
var searchResultType = graphql.NewObject(graphql.ObjectConfig{
	Name:        "SearchResult",
	Description: "A document with relevance scoring from a search query.",
	Fields: graphql.Fields{
		"id":           {Type: graphql.String},
		"source_type":  {Type: graphql.String},
		"source_id":    {Type: graphql.String},
		"title":        {Type: graphql.String},
		"content":      {Type: graphql.String},
		"metadata":     {Type: jsonScalar},
		"status":       {Type: graphql.String},
		"collected_at": {Type: graphql.String},
		"created_at":   {Type: graphql.String},
		"updated_at":   {Type: graphql.String},
		"score":        {Type: graphql.Float, Description: "Relevance score."},
		"match_type":   {Type: graphql.String, Description: "fulltext | vector | hybrid"},
	},
})

// curatedResultType mirrors curation.CuratedResult.
var curatedResultType = graphql.NewObject(graphql.ObjectConfig{
	Name:        "CuratedResult",
	Description: "An LLM-curated and re-ranked search result.",
	Fields: graphql.Fields{
		"summary":          {Type: graphql.String, Description: "LLM-generated summary."},
		"relevance":        {Type: graphql.Float, Description: "LLM-assigned relevance score."},
		"relevance_reason": {Type: graphql.String, Description: "Explanation of relevance."},
		"original": {
			Type:        documentType,
			Description: "The original document.",
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				if r, ok := p.Source.(curation.CuratedResult); ok {
					return docToMap(&r.Original), nil
				}
				return nil, nil
			},
		},
	},
})

// searchQueryResultType is the top-level return from the search query.
var searchQueryResultType = graphql.NewObject(graphql.ObjectConfig{
	Name:        "SearchQueryResult",
	Description: "Response envelope for a search query.",
	Fields: graphql.Fields{
		"results":  {Type: graphql.NewList(searchResultType), Description: "Ranked search results."},
		"curated":  {Type: graphql.NewList(curatedResultType), Description: "LLM-curated results (only when curated=true)."},
		"count":    {Type: graphql.Int},
		"query":    {Type: graphql.String},
		"is_curated": {Type: graphql.Boolean, Description: "True when LLM curation was applied."},
		"took_ms":  {Type: graphql.Int},
	},
})

// statsResultType mirrors the statsHandler JSON envelope.
var statsResultType = graphql.NewObject(graphql.ObjectConfig{
	Name:        "StatsResult",
	Description: "Document counts grouped by source type.",
	Fields: graphql.Fields{
		"by_source": {Type: jsonScalar, Description: "Map of source name to count."},
		"total":     {Type: graphql.Int},
	},
})

// feedbackInputType is the GraphQL input type for createFeedback.
var feedbackInputType = graphql.NewInputObject(graphql.InputObjectConfig{
	Name:        "FeedbackInput",
	Description: "Input payload for recording user feedback.",
	Fields: graphql.InputObjectConfigFieldMap{
		"query":       {Type: graphql.String},
		"documentId":  {Type: graphql.String},
		"chunkId":     {Type: graphql.Int},
		"source":      {Type: graphql.NewNonNull(graphql.String)},
		"sessionId":   {Type: graphql.String},
		"userId":      {Type: graphql.String},
		"thumbs":      {Type: graphql.NewNonNull(graphql.Int)},
		"comment":     {Type: graphql.String},
		"metadata":    {Type: jsonScalar},
	},
})

// feedbackResultType is returned by createFeedback.
var feedbackResultType = graphql.NewObject(graphql.ObjectConfig{
	Name:        "FeedbackResult",
	Description: "Result of recording feedback.",
	Fields: graphql.Fields{
		"id": {Type: graphql.Int, Description: "Generated feedback ID."},
	},
})

// --- resolve helpers ---

// docToMap converts model.Document to map[string]interface{} suitable for
// graphql-go field resolution. graphql-go resolves struct fields by name, but
// using maps is simpler and avoids reflection edge cases with embedded structs.
func docToMap(d *model.Document) map[string]interface{} {
	if d == nil {
		return nil
	}
	return map[string]interface{}{
		"id":           d.ID.String(),
		"source_type":  string(d.SourceType),
		"source_id":    d.SourceID,
		"title":        d.Title,
		"content":      d.Content,
		"metadata":     d.Metadata,
		"status":       d.Status,
		"collected_at": d.CollectedAt.Format(time.RFC3339),
		"created_at":   d.CreatedAt.Format(time.RFC3339),
		"updated_at":   d.UpdatedAt.Format(time.RFC3339),
	}
}

// searchResultToMap converts model.SearchResult to a flat map.
func searchResultToMap(sr *model.SearchResult) map[string]interface{} {
	if sr == nil {
		return nil
	}
	m := docToMap(&sr.Document)
	m["score"] = sr.Score
	m["match_type"] = sr.MatchType
	return m
}

// curatedToMap converts curation.CuratedResult to a map. The "original" field
// is resolved by the field resolver on curatedResultType above.
func curatedToMap(cr curation.CuratedResult) map[string]interface{} {
	return map[string]interface{}{
		"summary":          cr.Summary,
		"relevance":        cr.Relevance,
		"relevance_reason": cr.RelevanceReason,
		"original":         cr,
	}
}

// optString returns nil if s is empty, otherwise a pointer to s.
func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// optSourceType returns nil if s is empty, otherwise a pointer to a SourceType.
func optSourceType(s string) *model.SourceType {
	if s == "" {
		return nil
	}
	st := model.SourceType(s)
	return &st
}

// buildSchema constructs the GraphQL schema with all queries and mutations.
// It captures s (Server) in closures so resolver functions have full access to
// application dependencies without global state.
func (s *Server) buildSchema() (graphql.Schema, error) {
	queryFields := graphql.Fields{

		// --- search ---
		"search": {
			Type:        searchQueryResultType,
			Description: "Full-text / vector hybrid search over documents.",
			Args: graphql.FieldConfigArgument{
				"query":              {Type: graphql.NewNonNull(graphql.String)},
				"sourceType":         {Type: graphql.String},
				"excludeSourceTypes": {Type: graphql.NewList(graphql.NewNonNull(graphql.String))},
				"limit":              {Type: graphql.Int},
				"includeDeleted":     {Type: graphql.Boolean},
				"sort":               {Type: graphql.String},
				"useHyDE":            {Type: graphql.Boolean},
				"curated":            {Type: graphql.Boolean},
			},
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				queryStr, _ := p.Args["query"].(string)
				if queryStr == "" {
					return nil, fmt.Errorf("query argument is required")
				}

				var excludeSrcs []model.SourceType
				if raw, ok := p.Args["excludeSourceTypes"].([]interface{}); ok {
					for _, v := range raw {
						if sv, ok := v.(string); ok && sv != "" {
							excludeSrcs = append(excludeSrcs, model.SourceType(sv))
						}
					}
				}

				limit, _ := p.Args["limit"].(int)
				includeDeleted, _ := p.Args["includeDeleted"].(bool)
				sort, _ := p.Args["sort"].(string)
				useHyDE, _ := p.Args["useHyDE"].(bool)
				curated, _ := p.Args["curated"].(bool)
				sourceType, _ := p.Args["sourceType"].(string)

				q := model.SearchQuery{
					Query:              queryStr,
					SourceType:         optSourceType(sourceType),
					ExcludeSourceTypes: excludeSrcs,
					Limit:              limit,
					IncludeDeleted:     includeDeleted,
					Sort:               sort,
					UseHyDE:            useHyDE,
				}

				start := time.Now()
				results, err := s.search.Search(p.Context, q)
				if err != nil {
					slog.Error("graphql: search failed", "error", err)
					return nil, fmt.Errorf("internal server error")
				}

				tookMs := int(time.Since(start).Milliseconds())

				if curated {
					curator := curation.New(s.llmClient)
					curatedResults, err := curator.Curate(p.Context, queryStr, results)
					if err != nil {
						slog.Error("graphql: curation failed", "error", err)
						return nil, fmt.Errorf("curation failed")
					}
					mapped := make([]interface{}, len(curatedResults))
					for i, cr := range curatedResults {
						mapped[i] = curatedToMap(cr)
					}
					return map[string]interface{}{
						"results":    []interface{}{},
						"curated":    mapped,
						"count":      len(curatedResults),
						"query":      queryStr,
						"is_curated": true,
						"took_ms":    tookMs,
					}, nil
				}

				mapped := make([]interface{}, len(results))
				for i, r := range results {
					mapped[i] = searchResultToMap(r)
				}
				return map[string]interface{}{
					"results":    mapped,
					"curated":    []interface{}{},
					"count":      len(results),
					"query":      queryStr,
					"is_curated": false,
					"took_ms":    tookMs,
				}, nil
			},
		},

		// --- documents ---
		"documents": {
			Type:        graphql.NewList(documentType),
			Description: "List documents with optional source filtering.",
			Args: graphql.FieldConfigArgument{
				"source":        {Type: graphql.String},
				"excludeSource": {Type: graphql.String},
				"limit":         {Type: graphql.Int},
				"offset":        {Type: graphql.Int},
			},
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				limit, _ := p.Args["limit"].(int)
				if limit <= 0 {
					limit = 20
				}
				if limit > 100 {
					limit = 100
				}
				offset, _ := p.Args["offset"].(int)

				var includeSrc model.SourceType
				if v, _ := p.Args["source"].(string); v != "" {
					includeSrc = model.SourceType(v)
				}

				var excludeSrcs []model.SourceType
				if v, _ := p.Args["excludeSource"].(string); v != "" {
					for _, part := range strings.Split(v, ",") {
						part = strings.TrimSpace(part)
						if part != "" {
							excludeSrcs = append(excludeSrcs, model.SourceType(part))
						}
					}
				}

				docs, err := s.docs.ListRecent(p.Context, includeSrc, excludeSrcs, limit, offset)
				if err != nil {
					slog.Error("graphql: list documents failed", "error", err)
					return nil, fmt.Errorf("internal server error")
				}

				out := make([]interface{}, len(docs))
				for i, d := range docs {
					out[i] = docToMap(d)
				}
				return out, nil
			},
		},

		// --- document ---
		"document": {
			Type:        documentType,
			Description: "Fetch a single document by UUID.",
			Args: graphql.FieldConfigArgument{
				"id": {Type: graphql.NewNonNull(graphql.ID)},
			},
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				idStr, _ := p.Args["id"].(string)
				id, err := uuid.Parse(idStr)
				if err != nil {
					return nil, fmt.Errorf("invalid document ID")
				}

				doc, err := s.docs.GetByID(p.Context, id)
				if err != nil {
					return nil, fmt.Errorf("document not found")
				}
				return docToMap(doc), nil
			},
		},

		// --- sources ---
		"sources": {
			Type:        jsonScalar,
			Description: "Map of source name to document count.",
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				counts, err := s.docs.CountBySource(p.Context)
				if err != nil {
					slog.Error("graphql: count by source failed", "error", err)
					return nil, fmt.Errorf("internal server error")
				}
				return counts, nil
			},
		},

		// --- stats ---
		"stats": {
			Type:        statsResultType,
			Description: "Document counts grouped by source type (active only).",
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				counts, err := s.docs.CountBySource(p.Context)
				if err != nil {
					slog.Error("graphql: stats failed", "error", err)
					return nil, fmt.Errorf("internal server error")
				}

				known := []string{"filesystem", "slack", "github"}
				bySource := make(map[string]int, len(known))
				total := 0
				for _, k := range known {
					v := counts[k]
					bySource[k] = v
					total += v
				}
				for k, v := range counts {
					if _, ok := bySource[k]; !ok {
						bySource[k] = v
						total += v
					}
				}
				return map[string]interface{}{
					"by_source": bySource,
					"total":     total,
				}, nil
			},
		},

		// --- baselineStats ---
		"baselineStats": {
			Type:        jsonScalar,
			Description: "Detailed baseline metrics: doc counts, chunks, extraction failures, collection timestamps.",
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				stats, err := s.docs.QueryBaselineStats(p.Context)
				if err != nil {
					slog.Error("graphql: baseline stats failed", "error", err)
					return nil, fmt.Errorf("internal server error")
				}
				// Marshal then unmarshal to produce a plain map[string]interface{}
				// that the JSON scalar serializer can handle uniformly.
				b, err := json.Marshal(stats)
				if err != nil {
					return nil, fmt.Errorf("graphql: marshal baseline stats: %w", err)
				}
				var out interface{}
				if err := json.Unmarshal(b, &out); err != nil {
					return nil, fmt.Errorf("graphql: unmarshal baseline stats: %w", err)
				}
				return out, nil
			},
		},
	}

	mutationFields := graphql.Fields{

		// --- createFeedback ---
		"createFeedback": {
			Type:        feedbackResultType,
			Description: "Record user feedback for a search result or document.",
			Args: graphql.FieldConfigArgument{
				"input": {Type: graphql.NewNonNull(feedbackInputType)},
			},
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				inputRaw, ok := p.Args["input"].(map[string]interface{})
				if !ok {
					return nil, fmt.Errorf("invalid input")
				}

				source, _ := inputRaw["source"].(string)
				if source == "" {
					return nil, fmt.Errorf("source is required")
				}

				thumbsVal, _ := inputRaw["thumbs"].(int)
				thumbs := int16(thumbsVal)
				if thumbs < -1 || thumbs > 1 {
					return nil, fmt.Errorf("thumbs must be -1, 0, or 1")
				}

				var chunkID *int64
				if v, ok := inputRaw["chunkId"].(int); ok {
					cv := int64(v)
					chunkID = &cv
				}

				var meta map[string]any
				if v, ok := inputRaw["metadata"].(map[string]interface{}); ok {
					meta = v
				}
				if meta == nil {
					meta = map[string]any{}
				}

				f := store.Feedback{
					Query:      optString(stringFromMap(inputRaw, "query")),
					DocumentID: optString(stringFromMap(inputRaw, "documentId")),
					ChunkID:    chunkID,
					Source:     source,
					SessionID:  optString(stringFromMap(inputRaw, "sessionId")),
					UserID:     optString(stringFromMap(inputRaw, "userId")),
					Thumbs:     thumbs,
					Comment:    optString(stringFromMap(inputRaw, "comment")),
					Metadata:   meta,
				}

				id, err := s.feedback.Record(p.Context, f)
				if err != nil {
					slog.Error("graphql: record feedback failed", "error", err)
					return nil, fmt.Errorf("internal server error")
				}

				return map[string]interface{}{"id": int(id)}, nil
			},
		},
	}

	rootQuery := graphql.NewObject(graphql.ObjectConfig{
		Name:   "Query",
		Fields: queryFields,
	})

	rootMutation := graphql.NewObject(graphql.ObjectConfig{
		Name:   "Mutation",
		Fields: mutationFields,
	})

	return graphql.NewSchema(graphql.SchemaConfig{
		Query:    rootQuery,
		Mutation: rootMutation,
	})
}

// stringFromMap safely extracts a string value from a map, returning "" if
// the key is absent or the value is not a string.
func stringFromMap(m map[string]interface{}, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

// graphqlHandler builds the GraphQL schema and returns an http.Handler that
// serves both GET and POST requests at /api/v1/graphql.
// The GraphiQL playground is enabled (serves HTML when Accept: text/html).
func (s *Server) graphqlHandler() http.Handler {
	schema, err := s.buildSchema()
	if err != nil {
		// Schema construction failures are programming errors; panic to surface
		// them immediately at startup rather than silently serving broken responses.
		panic(fmt.Sprintf("graphql: failed to build schema: %v", err))
	}

	h := gqlhandler.New(&gqlhandler.Config{
		Schema:     &schema,
		Pretty:     false,
		GraphiQL:   true,
		Playground: false,
	})

	return h
}
