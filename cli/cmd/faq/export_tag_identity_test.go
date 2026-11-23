package faq

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/Tencent/WeKnora/client"
)

// Tag identity travels by name, not by id.
//
// The server resolves an import payload's tag by preferring tag_id (seq_id)
// when that id exists AND belongs to the target knowledge base, and only
// falling back to tag_name otherwise (internal/application/service/
// knowledge_faq.go resolveTagID). A snapshot that carries tag_id is therefore
// unsafe across a tenant/KB migration: the source seq_id can collide with a
// same-numbered but different-meaning tag in the destination, and the server's
// id-first resolution would silently file the entry under the wrong tag.
// tag_name is the identity that survives the move -- same-tenant round trips
// resolve it back to the original id via FindOrCreateTagByName.

// tagStoreService is a small in-memory FAQ backend whose Upsert mimics the
// server's tag-resolution priority: tag_id wins when it exists in the store,
// tag_name is the fallback.
type tagStoreService struct {
	entries []sdk.FAQEntry
	tags    map[int64]string // seq_id -> name, the destination tenant's tags
	// resolved records, per imported entry, the tag seq_id the store's
	// resolution picked.
	resolved []int64
}

func (s *tagStoreService) ListFAQEntries(_ context.Context, _ string, _, _ int, _ int64, _, _, _ string) (*sdk.FAQEntriesPage, error) {
	return &sdk.FAQEntriesPage{Total: int64(len(s.entries)), Entries: s.entries}, nil
}

func (s *tagStoreService) UpsertFAQEntries(_ context.Context, _ string, payload *sdk.FAQBatchUpsertPayload) (string, error) {
	for _, e := range payload.Entries {
		// resolveTagID's order: id first if it exists here, else name.
		if _, ok := s.tags[e.TagID]; ok && e.TagID != 0 {
			s.resolved = append(s.resolved, e.TagID)
			continue
		}
		for seq, name := range s.tags {
			if name == e.TagName {
				s.resolved = append(s.resolved, seq)
				break
			}
		}
	}
	return "task-a", nil
}

func (s *tagStoreService) GetFAQImportProgress(context.Context, string) (*sdk.FAQImportProgress, error) {
	panic("not used")
}

func TestExportSnapshotTagIdentityTravelsByNameNotByID(t *testing.T) {
	entry := sdk.FAQEntry{
		ID:               11,
		StandardQuestion: "怎么退货",
		Answers:          []string{"联系客服"},
		TagID:            7,
		TagName:          "退货",
	}
	snap, err := exportEntries(context.Background(), &pagedService{
		pages: [][]sdk.FAQEntry{{entry}},
		total: 1,
	}, "kb-1")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(snap) != 1 {
		t.Fatalf("snapshot has %d entries, want 1", len(snap))
	}
	if snap[0].TagID != 0 {
		t.Fatalf("snapshot carries tag_id %d; tag identity must travel by name, not by id", snap[0].TagID)
	}
	if snap[0].TagName != "退货" {
		t.Fatalf("snapshot lost tag_name: %q", snap[0].TagName)
	}
	// The JSON the snapshot file actually contains must not carry the key.
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), `"tag_id"`) {
		t.Fatalf("exported JSON still contains a tag_id key: %s", data)
	}
	if !strings.Contains(string(data), `"tag_name":"退货"`) {
		t.Fatalf("exported JSON lost tag_name: %s", data)
	}
}

// A same-tenant export -> import round trip must land the entry on the tag it
// came from: the server resolves the name back to the original seq_id.
func TestExportImportRoundTripKeepsTheTag(t *testing.T) {
	store := &tagStoreService{
		entries: []sdk.FAQEntry{{
			ID: 1, StandardQuestion: "q", Answers: []string{"a"},
			TagID: 7, TagName: "退货",
		}},
		tags: map[int64]string{7: "退货", 8: "物流"},
	}
	snap, err := exportEntries(context.Background(), store, "kb-1")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if _, err := store.UpsertFAQEntries(context.Background(), "kb-1",
		&sdk.FAQBatchUpsertPayload{Mode: "append", Entries: snap}); err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(store.resolved) != 1 || store.resolved[0] != 7 {
		t.Fatalf("round trip resolved tag %v, want the original [7]", store.resolved)
	}
}

// Across a tenant/KB migration, a stale tag_id that happens to exist in the
// destination means a different tag. A name-only snapshot cannot be hijacked
// by that collision.
func TestExportSnapshotSurvivesATagIDCollisionInAnotherTenant(t *testing.T) {
	src, err := exportEntries(context.Background(), &pagedService{
		pages: [][]sdk.FAQEntry{{{
			ID: 1, StandardQuestion: "q", Answers: []string{"a"},
			TagID: 7, TagName: "退货",
		}}},
		total: 1,
	}, "kb-src")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	// Destination: seq_id 7 is a different tag, "物流" lives elsewhere.
	dst := &tagStoreService{tags: map[int64]string{7: "会员", 9: "退货"}}
	if _, err := dst.UpsertFAQEntries(context.Background(), "kb-dst",
		&sdk.FAQBatchUpsertPayload{Mode: "append", Entries: src}); err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(dst.resolved) != 1 || dst.resolved[0] != 9 {
		t.Fatalf("migrated entry landed on tag %v, want 9 (\"退货\") -- a carried tag_id 7 would have been resolved id-first onto \"会员\"", dst.resolved)
	}
}
