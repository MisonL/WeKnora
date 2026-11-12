package types

import (
	"strings"
	"testing"
)

func TestIsGeneratedQuestionSource(t *testing.T) {
	const chunkID = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
	cases := []struct {
		name       string
		chunkID    string
		sourceID   string
		sourceType SourceType
		want       bool
	}{
		{name: "body row", chunkID: chunkID, sourceID: chunkID, sourceType: ChunkSourceType},
		{
			name: "question row", chunkID: chunkID, sourceID: chunkID + "-q123",
			sourceType: ChunkSourceType, want: true,
		},
		{
			name: "hashed question row", chunkID: chunkID, sourceID: chunkID + "-q0123456789abcdef01234567",
			sourceType: ChunkSourceType, want: true,
		},
		{
			name: "image row with a suffix", chunkID: chunkID, sourceID: chunkID + "-image",
			sourceType: ImageSourceType,
		},
		{name: "empty chunk", sourceID: "-q1", sourceType: ChunkSourceType},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsGeneratedQuestionSource(tc.chunkID, tc.sourceID, tc.sourceType); got != tc.want {
				t.Fatalf("IsGeneratedQuestionSource() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestQuestionGenerationOffIDs(t *testing.T) {
	off := &KnowledgeBase{ID: "off", Type: KnowledgeBaseTypeDocument}
	on := &KnowledgeBase{
		ID: "on", Type: KnowledgeBaseTypeDocument,
		QuestionGenerationConfig: &QuestionGenerationConfig{Enabled: true},
	}
	got := QuestionGenerationOffIDs([]*KnowledgeBase{off, on, nil})
	if _, ok := got["off"]; !ok {
		t.Fatal("closed document knowledge base was not marked")
	}
	if _, ok := got["on"]; ok {
		t.Fatal("enabled knowledge base was marked off")
	}
}

func TestQuestionGenerationDisabledLeavesFAQAlone(t *testing.T) {
	faq := &KnowledgeBase{Type: KnowledgeBaseTypeFAQ}
	if QuestionGenerationDisabled(faq) {
		t.Fatal("FAQ similar-question rows must not be treated as generated questions")
	}
	off := &KnowledgeBase{Type: KnowledgeBaseTypeDocument}
	if !QuestionGenerationDisabled(off) {
		t.Fatal("a document knowledge base with no question config is off")
	}
	on := &KnowledgeBase{
		Type:                     KnowledgeBaseTypeDocument,
		QuestionGenerationConfig: &QuestionGenerationConfig{Enabled: true},
	}
	if QuestionGenerationDisabled(on) || !QuestionGenerationActive(on) {
		t.Fatal("an enabled document knowledge base should keep generated questions")
	}
}

func TestGeneratedQuestionRowPredicateParadeDBAvoidsColumnComparison(t *testing.T) {
	sql := GeneratedQuestionRowPredicateParadeDB("")
	if !strings.Contains(sql, "starts_with(source_id, chunk_id || '-')") {
		t.Fatalf("predicate = %q, want a starts_with prefix test", sql)
	}
	// A column-to-column comparison anywhere in this filter makes ParadeDB
	// reject the whole top-K BM25 query with "Unsupported query shape".
	for _, forbidden := range []string{"source_id <>", "substr("} {
		if strings.Contains(sql, forbidden) {
			t.Fatalf("predicate still contains %q: %s", forbidden, sql)
		}
	}
	if qualified := GeneratedQuestionRowPredicateParadeDB("embeddings"); !strings.Contains(
		qualified, "starts_with(embeddings.source_id, embeddings.chunk_id || '-')") {
		t.Fatalf("qualified predicate = %q", qualified)
	}
}
