package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// A failing tag lookup must surface as an error, not as a page of entries with
// empty tag names: an export built on that page carries no tag identity at all
// and would re-import every entry as uncategorised.
func TestFAQTagNameMapsSurfaceLookupFailures(t *testing.T) {
	want := errors.New("tag store unavailable")
	svc := &knowledgeService{tagRepo: &failingTagLookupRepo{err: want}}

	_, _, err := svc.faqTagNameMaps(context.Background(), 7, []string{"tag-1"})
	require.ErrorIs(t, err, want)
}

// The happy path still resolves both maps, and the "no tags referenced" case
// needs no repository at all.
func TestFAQTagNameMapsResolveNamesAndSeqIDs(t *testing.T) {
	svc := &knowledgeService{tagRepo: &failingTagLookupRepo{tags: []*types.KnowledgeTag{
		{ID: "tag-1", Name: "退款", SeqID: 11},
	}}}

	names, seqIDs, err := svc.faqTagNameMaps(context.Background(), 7, []string{"tag-1"})
	require.NoError(t, err)
	require.Equal(t, "退款", names["tag-1"])
	require.EqualValues(t, 11, seqIDs["tag-1"])

	svc = &knowledgeService{}
	names, seqIDs, err = svc.faqTagNameMaps(context.Background(), 7, []string{"tag-1"})
	require.NoError(t, err)
	require.Empty(t, names)
	require.Empty(t, seqIDs)
}

type failingTagLookupRepo struct {
	interfaces.KnowledgeTagRepository
	tags []*types.KnowledgeTag
	err  error
}

func (r *failingTagLookupRepo) GetByIDs(context.Context, uint64, []string) ([]*types.KnowledgeTag, error) {
	return r.tags, r.err
}
