package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func serveFAQProgress(t *testing.T, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL)
}

// A completed import's record is the only place a caller learns that entries
// imported with questions stripped: the server moves the inline failed_entries
// list into a CSV as soon as there is one, so partial_failed_count is all that
// is left to count them by. Dropping the field turns a degraded import into a
// clean-looking one.
func TestFAQImportProgressDecodesTheWholeRecord(t *testing.T) {
	const body = `{"success":true,"data":{
		"task_id":"task-1","kb_id":"kb-1","knowledge_id":"kn-1","status":"completed",
		"progress":100,"total":10,"processed":10,
		"success_count":6,"failed_count":2,"partial_failed_count":1,"skipped_count":1,
		"failed_entries_url":"faq_import/task-1_failed.csv",
		"valid_entry_indices":[0,1,2,3,4,5,6],
		"merge_entry_indices":[2,3],
		"next_entry_index":7,
		"merged_count":2,"added_count":4,
		"merge_details":[{"index":2,"standard_question":"How do I reset it?","answer_changed":true,"new_similar_count":1,"new_negative_count":0}],
		"message":"导入完成","created_at":1,"updated_at":2,
		"import_mode":"append","display_status":"open","processing_time":1500
	}}`

	p, err := serveFAQProgress(t, body).GetFAQImportProgress(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("get progress: %v", err)
	}
	if p.PartialFailedCount != 1 {
		t.Fatalf("PartialFailedCount = %d, want 1", p.PartialFailedCount)
	}
	if p.SkippedCount != 1 {
		t.Fatalf("SkippedCount = %d, want 1", p.SkippedCount)
	}
	if got := p.ValidEntryIndices; len(got) != 7 || got[6] != 6 {
		t.Fatalf("ValidEntryIndices = %v, want 0..6", got)
	}
	if got := p.MergeEntryIndices; len(got) != 2 || got[1] != 3 {
		t.Fatalf("MergeEntryIndices = %v, want [2 3]", got)
	}
	if p.NextEntryIndex != 7 {
		t.Fatalf("NextEntryIndex = %d, want 7", p.NextEntryIndex)
	}
	if p.MergedCount != 2 || p.AddedCount != 4 {
		t.Fatalf("MergedCount/AddedCount = %d/%d, want 2/4", p.MergedCount, p.AddedCount)
	}
	if len(p.MergeDetails) != 1 || p.MergeDetails[0].StandardQuestion != "How do I reset it?" ||
		!p.MergeDetails[0].AnswerChanged || p.MergeDetails[0].NewSimilarCount != 1 {
		t.Fatalf("MergeDetails = %+v", p.MergeDetails)
	}
	if p.ImportMode != "append" || p.DisplayStatus != "open" || p.ProcessingTime != 1500 {
		t.Fatalf("result fields = %q/%q/%d", p.ImportMode, p.DisplayStatus, p.ProcessingTime)
	}
}

// The count fields carry omitempty, so a clean import sends none of them.
// Absence means zero here, never "an older server that cannot report it" --
// which is why the struct has no pointer fields to make the two distinguishable.
func TestFAQImportProgressTreatsAbsentCountsAsZero(t *testing.T) {
	const body = `{"success":true,"data":{
		"task_id":"task-2","kb_id":"kb-1","knowledge_id":"kn-1","status":"completed",
		"progress":100,"total":3,"processed":3,"success_count":3,
		"message":"导入完成","created_at":1,"updated_at":2
	}}`

	p, err := serveFAQProgress(t, body).GetFAQImportProgress(context.Background(), "task-2")
	if err != nil {
		t.Fatalf("get progress: %v", err)
	}
	if p.FailedCount != 0 || p.PartialFailedCount != 0 || p.SkippedCount != 0 {
		t.Fatalf("counts = %d/%d/%d, want zeros", p.FailedCount, p.PartialFailedCount, p.SkippedCount)
	}
	if p.ValidEntryIndices != nil || p.MergeDetails != nil {
		t.Fatalf("list fields = %v/%v, want nil", p.ValidEntryIndices, p.MergeDetails)
	}
}

// failed_entries entries also distinguish a partial failure, which matters when
// the list is small enough to still be inline.
func TestFAQFailedEntryDecodesPartialFailureDetail(t *testing.T) {
	const body = `{"success":true,"data":{
		"task_id":"task-3","status":"completed","progress":100,"total":1,
		"failed_entries":[{
			"index":0,"reason":"部分成功：1条相似问被移除",
			"failure_type":"pre_validation","is_partial_failure":true,
			"standard_question":"q","removed_similar_questions":["conflict"],
			"removed_negative_questions":["neg"],
			"message":"","created_at":1,"updated_at":2
		}]
	}}`

	p, err := serveFAQProgress(t, body).GetFAQImportProgress(context.Background(), "task-3")
	if err != nil {
		t.Fatalf("get progress: %v", err)
	}
	if len(p.FailedEntries) != 1 {
		t.Fatalf("%d failed entries, want 1", len(p.FailedEntries))
	}
	fe := p.FailedEntries[0]
	if !fe.IsPartialFailure || fe.FailureType != "pre_validation" {
		t.Fatalf("entry = %+v", fe)
	}
	if len(fe.RemovedSimilarQuestions) != 1 || len(fe.RemovedNegativeQuestions) != 1 {
		t.Fatalf("removed questions = %v/%v", fe.RemovedSimilarQuestions, fe.RemovedNegativeQuestions)
	}
}
