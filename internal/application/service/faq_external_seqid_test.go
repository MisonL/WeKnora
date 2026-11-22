package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	secutils "github.com/Tencent/WeKnora/internal/utils"
)

// seqIDProbe is a chunk repository whose only real method answers
// SeqIDsInUse; everything else is the embedded nil interface.
type seqIDProbe struct {
	interfaces.ChunkRepository
	taken map[int64]bool
	err   error
	calls int
}

func (p *seqIDProbe) SeqIDsInUse(_ context.Context, ids []int64) (map[int64]bool, error) {
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	inUse := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if p.taken[id] {
			inUse[id] = true
		}
	}
	return inUse, nil
}

func idPtr(v int64) *int64 { return &v }

// A seq id is globally unique, so a preserved id collides with the very row it
// was exported from (an export/import round trip) and fails the whole insert
// batch. Taken, duplicated and out-of-range ids are dropped; a free one kept.
func TestImportDropsSeqIDsThatWouldCollide(t *testing.T) {
	probe := &seqIDProbe{taken: map[int64]bool{7001: true}}
	svc := &knowledgeService{chunkRepo: probe}
	entries := []*types.FAQEntryPayload{
		{ID: idPtr(7001), StandardQuestion: "taken"},
		{ID: idPtr(7002), StandardQuestion: "free"},
		{ID: idPtr(7002), StandardQuestion: "duplicate of the free one"},
		{ID: idPtr(types.FAQExternalSeqIDLimit), StandardQuestion: "at the sequence start"},
		{ID: idPtr(0), StandardQuestion: "zero"},
		{ID: idPtr(-5), StandardQuestion: "negative"},
		{StandardQuestion: "no id at all"},
	}

	svc.dropUnusableExternalSeqIDs(context.Background(), entries, false)

	require.Nil(t, entries[0].ID, "an id another row holds can never be written")
	require.NotNil(t, entries[1].ID, "a free id below the limit is honoured")
	require.Equal(t, int64(7002), *entries[1].ID)
	require.Nil(t, entries[2].ID, "the second claim on the same id would collide in the same batch")
	require.Nil(t, entries[3].ID, "the sequence will hand this value out later")
	require.Nil(t, entries[4].ID)
	require.Nil(t, entries[5].ID)
	require.Nil(t, entries[6].ID)
	require.Equal(t, 1, probe.calls, "the check is one bulk query, not one per entry")
}

// Replace mode creates the new rows before deleting the old ones, so every
// preserved id would collide with the row it came from.
func TestImportDropsAllSeqIDsInReplaceMode(t *testing.T) {
	probe := &seqIDProbe{}
	svc := &knowledgeService{chunkRepo: probe}
	entries := []*types.FAQEntryPayload{
		{ID: idPtr(100), StandardQuestion: "a"},
		{ID: idPtr(200), StandardQuestion: "b"},
	}

	svc.dropUnusableExternalSeqIDs(context.Background(), entries, true)

	require.Nil(t, entries[0].ID)
	require.Nil(t, entries[1].ID)
	require.Equal(t, 0, probe.calls, "replace mode needs no lookup at all")
}

// If the lookup fails, the import falls back to database-assigned ids instead
// of guessing and failing the batch on a unique violation.
func TestImportDropsSeqIDsWhenTheLookupFails(t *testing.T) {
	probe := &seqIDProbe{err: errors.New("db down")}
	svc := &knowledgeService{chunkRepo: probe}
	entry := &types.FAQEntryPayload{ID: idPtr(500), StandardQuestion: "a"}

	svc.dropUnusableExternalSeqIDs(context.Background(), []*types.FAQEntryPayload{entry}, false)

	require.Nil(t, entry.ID)
}

// The single-create path follows the same rule.
func TestSingleCreateHonoursOnlyFreeSeqIDs(t *testing.T) {
	probe := &seqIDProbe{taken: map[int64]bool{42: true}}
	svc := &knowledgeService{chunkRepo: probe}
	ctx := context.Background()

	require.Equal(t, int64(0), svc.externalFAQSeqID(ctx, nil))
	require.Equal(t, int64(0), svc.externalFAQSeqID(ctx, idPtr(42)), "taken")
	require.Equal(t, int64(0), svc.externalFAQSeqID(ctx, idPtr(types.FAQExternalSeqIDLimit)))
	require.Equal(t, int64(0), svc.externalFAQSeqID(ctx, idPtr(-1)))
	require.Equal(t, int64(43), svc.externalFAQSeqID(ctx, idPtr(43)))
}

// CWE-1236: a cell starting with =, +, -, @, tab or CR is a formula to
// spreadsheet software. The export has to neutralize it and the importer has
// to strip the marker back off, or the round trip changes the text.
func TestCSVExportNeutralisesFormulaCells(t *testing.T) {
	for _, field := range []string{"=1+1", "+cmd|' /C calc'!A0", "-2+3", "@SUM(1)", "\tlead", "\rlead"} {
		out := escapeCSVField(field)
		require.True(t, strings.HasPrefix(strings.Trim(out, "\""), "'"),
			"a cell starting with %q must be prefixed, got %q", field, out)
	}
	require.Equal(t, "plain text", escapeCSVField("plain text"))
	require.Equal(t, "a=b", escapeCSVField("a=b"), "only a leading character is dangerous")
	// An existing apostrophe before a formula character is marked a second
	// time, so stripping one on import restores the original text.
	require.Equal(t, `''=x`, escapeCSVField("'=x"))
	require.Equal(t, `'quoted`, escapeCSVField("'quoted"))
	// A field with a comma still gets quoted after the prefix.
	require.Equal(t, `"'=a,b"`, escapeCSVField("=a,b"))
}

// The progress route compares the tenant embedded in the task id with the
// caller's. Importing into a shared KB runs as the KB's owner, so minting the
// id with that tenant made the initiator's own poll a 404; the id has to carry
// the caller's tenant, with the KB's as the fallback for tenant-less paths.
func TestFAQImportTaskIDCarriesTheCallersTenant(t *testing.T) {
	require.Equal(t, uint64(7), faqImportTaskTenant(7, 3), "the initiator keeps their own tenant")
	require.Equal(t, uint64(3), faqImportTaskTenant(0, 3), "no caller tenant: use the knowledge base's")

	id := secutils.GenerateTaskID("faq_import", faqImportTaskTenant(7, 3), "kb-1")
	require.NoError(t, secutils.ValidateTaskID(id))
	taskTenant, err := secutils.TaskTenantID(id)
	require.NoError(t, err)
	require.Equal(t, uint64(7), taskTenant, "the id handed back must authorize the poll that returns it")
}
