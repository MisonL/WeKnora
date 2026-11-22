package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// tagSeqProbe is a tag repository that answers GetBySeqID from a fixture.
type tagSeqProbe struct {
	interfaces.KnowledgeTagRepository
	tags    map[int64]*types.KnowledgeTag
	asked   []int64
	failIDs map[int64]bool
}

func (p *tagSeqProbe) GetBySeqID(_ context.Context, _ uint64, seqID int64) (*types.KnowledgeTag, error) {
	p.asked = append(p.asked, seqID)
	if p.failIDs[seqID] {
		return nil, errors.New("database is down")
	}
	if tag, ok := p.tags[seqID]; ok {
		return tag, nil
	}
	return nil, errors.New("tag not found")
}

// A tag seq id names a tag in one knowledge base. Exporting from one KB and
// importing into another carries ids that are out of scope in the target and
// would refuse the whole import at admission -- although every entry names its
// tag. Those ids must be cleared (the name resolves the tag), while an id with
// no name is left for the planner to report.
func TestImportNormalisesForeignTagIDs(t *testing.T) {
	repo := &tagSeqProbe{tags: map[int64]*types.KnowledgeTag{
		5:   {ID: "tag-a", SeqID: 5, TenantID: 1, KnowledgeBaseID: "kb-a", Name: "chitchat"},
		6:   {ID: "tag-b", SeqID: 6, TenantID: 1, KnowledgeBaseID: "kb-b", Name: "chitchat"},
		100: {ID: "tag-x", SeqID: 100, TenantID: 2, KnowledgeBaseID: "kb-b", Name: "foreign tenant"},
	}}
	entries := []types.FAQEntryPayload{
		{TagID: 5, TagName: "chitchat"},   // exported from kb-a: name fallback
		{TagID: 6, TagName: "chitchat"},   // already the target's tag: kept
		{TagID: 5, TagName: "chitchat"},   // same id again: cached, not re-asked
		{TagID: 7},                        // not found and unnamed: planner reports it
		{TagID: 100, TagName: "chitchat"}, // another tenant's: name fallback
		{TagID: 0, TagName: "chitchat"},   // nothing to do
	}

	normaliseFAQImportTagIDs(context.Background(), repo, 1, "kb-b", entries)

	require.Zero(t, entries[0].TagID, "a tag from the source KB must not block a migration")
	require.Equal(t, "chitchat", entries[0].TagName)
	require.Equal(t, int64(6), entries[1].TagID, "an in-scope tag is used as given")
	require.Zero(t, entries[2].TagID)
	require.Equal(t, int64(7), entries[3].TagID, "an unnamed id stays, so the refusal still names it")
	require.Zero(t, entries[4].TagID, "another tenant's tag id is not usable")
	require.Equal(t, []int64{5, 6, 7, 100}, repo.asked, "one lookup per distinct id")
}

// A lookup that fails for a reason other than "not found" says nothing about
// the tag. Clearing the id would turn "reuse this tag" into "create a tag with
// this name"; the planner's own lookup reports the outage instead.
func TestImportKeepsATagIDItCouldNotLookUp(t *testing.T) {
	repo := &tagSeqProbe{
		tags:    map[int64]*types.KnowledgeTag{},
		failIDs: map[int64]bool{9: true},
	}
	entries := []types.FAQEntryPayload{{TagID: 9, TagName: "chitchat"}}

	normaliseFAQImportTagIDs(context.Background(), repo, 1, "kb-b", entries)

	require.Equal(t, int64(9), entries[0].TagID, "a failed lookup must not silently switch to the name")
}
