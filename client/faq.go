package client

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// FAQEntry represents a FAQ item stored under a knowledge base.
type FAQEntry struct {
	ID                int64     `json:"id"`
	ChunkID           string    `json:"chunk_id"`
	KnowledgeID       string    `json:"knowledge_id"`
	KnowledgeBaseID   string    `json:"knowledge_base_id"`
	TagID             int64     `json:"tag_id"`
	TagName           string    `json:"tag_name"`
	IsEnabled         bool      `json:"is_enabled"`
	IsRecommended     bool      `json:"is_recommended"`
	StandardQuestion  string    `json:"standard_question"`
	SimilarQuestions  []string  `json:"similar_questions"`
	NegativeQuestions []string  `json:"negative_questions"`
	Answers           []string  `json:"answers"`
	AnswerStrategy    string    `json:"answer_strategy"`
	IndexMode         string    `json:"index_mode"`
	UpdatedAt         time.Time `json:"updated_at"`
	CreatedAt         time.Time `json:"created_at"`
	Score             float64   `json:"score,omitempty"`
	MatchType         string    `json:"match_type,omitempty"`
	ChunkType         string    `json:"chunk_type"`
	// MatchedQuestion is the actual question text that was matched in FAQ search
	// Could be the standard question or one of the similar questions
	MatchedQuestion string `json:"matched_question,omitempty"`
}

// FAQEntryPayload is used to create or update a FAQ entry.
type FAQEntryPayload struct {
	// ID is optional, used for data migration to specify seq_id (must be less than auto-increment start value 100000000)
	ID                *int64   `json:"id,omitempty"`
	StandardQuestion  string   `json:"standard_question"`
	SimilarQuestions  []string `json:"similar_questions,omitempty"`
	NegativeQuestions []string `json:"negative_questions,omitempty"`
	Answers           []string `json:"answers"`
	AnswerStrategy    *string  `json:"answer_strategy,omitempty"`
	TagID             int64    `json:"tag_id,omitempty"`
	TagName           string   `json:"tag_name,omitempty"`
	IsEnabled         *bool    `json:"is_enabled,omitempty"`
	IsRecommended     *bool    `json:"is_recommended,omitempty"`
}

// FAQBatchUpsertPayload represents the request body for batch import (append/replace).
type FAQBatchUpsertPayload struct {
	Entries     []FAQEntryPayload `json:"entries"`
	Mode        string            `json:"mode"`
	KnowledgeID string            `json:"knowledge_id,omitempty"`
	TaskID      string            `json:"task_id,omitempty"` // Optional, if not provided, a UUID will be generated
	DryRun      bool              `json:"dry_run,omitempty"` // If true, only validate without importing
}

// FAQEntryFieldsUpdate represents the fields that can be updated for a single FAQ entry.
type FAQEntryFieldsUpdate struct {
	IsEnabled     *bool  `json:"is_enabled,omitempty"`
	IsRecommended *bool  `json:"is_recommended,omitempty"`
	TagID         *int64 `json:"tag_id,omitempty"`
}

// FAQEntryFieldsBatchRequest updates multiple fields for FAQ entries in bulk.
// Supports two modes:
// 1. By entry ID: use ByID field
// 2. By Tag: use ByTag field to apply the same update to all entries under a tag
type FAQEntryFieldsBatchRequest struct {
	// ByID updates by entry ID (seq_id), key is entry seq_id
	ByID map[int64]FAQEntryFieldsUpdate `json:"by_id,omitempty"`
	// ByTag updates all entries under a tag, key is tag seq_id (0 for uncategorized)
	ByTag map[int64]FAQEntryFieldsUpdate `json:"by_tag,omitempty"`
	// ExcludeIDs IDs (seq_id) to exclude from the ByTag update
	ExcludeIDs []int64 `json:"exclude_ids,omitempty"`
}

// FAQEntryTagBatchRequest updates tags in bulk.
// key: entry seq_id, value: tag seq_id (nil to remove tag)
type FAQEntryTagBatchRequest struct {
	Updates map[int64]*int64 `json:"updates"`
}

// FAQDeleteRequest deletes entries in bulk.
type FAQDeleteRequest struct {
	IDs []int64 `json:"ids"`
}

// FAQSearchRequest represents the hybrid FAQ search request.
type FAQSearchRequest struct {
	QueryText            string  `json:"query_text"`
	VectorThreshold      float64 `json:"vector_threshold"`
	MatchCount           int     `json:"match_count"`
	FirstPriorityTagIDs  []int64 `json:"first_priority_tag_ids"`  // First priority tag seq_ids, highest priority
	SecondPriorityTagIDs []int64 `json:"second_priority_tag_ids"` // Second priority tag seq_ids, lower than first
	OnlyRecommended      bool    `json:"only_recommended"`        // Only return recommended entries
}

// FAQEntriesPage contains paginated FAQ results.
type FAQEntriesPage struct {
	Total    int64      `json:"total"`
	Page     int        `json:"page"`
	PageSize int        `json:"page_size"`
	Entries  []FAQEntry `json:"data"`
}

// FAQEntriesResponse wraps the paginated FAQ response.
type FAQEntriesResponse struct {
	Success bool            `json:"success"`
	Data    *FAQEntriesPage `json:"data"`
	Message string          `json:"message,omitempty"`
	Code    string          `json:"code,omitempty"`
}

// FAQUpsertResponse wraps the asynchronous import response.
type FAQUpsertResponse struct {
	Success bool            `json:"success"`
	Data    *FAQTaskPayload `json:"data"`
	Message string          `json:"message,omitempty"`
	Code    string          `json:"code,omitempty"`
}

// FAQTaskPayload carries the task identifier for async imports.
type FAQTaskPayload struct {
	TaskID string `json:"task_id"`
}

// FAQSearchResponse wraps the hybrid FAQ search results.
type FAQSearchResponse struct {
	Success bool       `json:"success"`
	Data    []FAQEntry `json:"data"`
	Message string     `json:"message,omitempty"`
	Code    string     `json:"code,omitempty"`
}

// FAQEntryResponse wraps the single FAQ entry creation response.
type FAQEntryResponse struct {
	Success bool      `json:"success"`
	Data    *FAQEntry `json:"data"`
	Message string    `json:"message,omitempty"`
	Code    string    `json:"code,omitempty"`
}

type faqSimpleResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
	Code    string `json:"code,omitempty"`
}

// ListFAQEntries returns paginated FAQ entries under a knowledge base.
// tagSeqID: filter by tag seq_id (0 means no filter)
// searchField: specifies which field to search in ("standard_question", "similar_questions", "answers", "" for all)
// sortOrder: "asc" for time ascending (updated_at ASC), default is time descending (updated_at DESC)
func (c *Client) ListFAQEntries(ctx context.Context,
	knowledgeBaseID string, page, pageSize int, tagSeqID int64, keyword string, searchField string, sortOrder string,
) (*FAQEntriesPage, error) {
	path := fmt.Sprintf("/api/v1/knowledge-bases/%s/faq/entries", knowledgeBaseID)
	query := url.Values{}
	if page > 0 {
		query.Add("page", strconv.Itoa(page))
	}
	if pageSize > 0 {
		query.Add("page_size", strconv.Itoa(pageSize))
	}
	if tagSeqID != 0 {
		query.Add("tag_id", strconv.FormatInt(tagSeqID, 10))
	}
	if keyword != "" {
		query.Add("keyword", keyword)
	}
	if searchField != "" {
		query.Add("search_field", searchField)
	}
	if sortOrder != "" {
		query.Add("sort_order", sortOrder)
	}

	resp, err := c.doRequest(ctx, http.MethodGet, path, nil, query)
	if err != nil {
		return nil, err
	}

	var response FAQEntriesResponse
	if err := parseResponse(resp, &response); err != nil {
		return nil, err
	}
	if response.Data == nil {
		return &FAQEntriesPage{}, nil
	}
	return response.Data, nil
}

// UpsertFAQEntries imports or appends FAQ entries asynchronously and returns the task ID.
func (c *Client) UpsertFAQEntries(ctx context.Context,
	knowledgeBaseID string, payload *FAQBatchUpsertPayload,
) (string, error) {
	path := fmt.Sprintf("/api/v1/knowledge-bases/%s/faq/entries", knowledgeBaseID)
	resp, err := c.doRequest(ctx, http.MethodPost, path, payload, nil)
	if err != nil {
		return "", err
	}

	var response FAQUpsertResponse
	if err := parseResponse(resp, &response); err != nil {
		return "", err
	}
	if response.Data == nil {
		return "", fmt.Errorf("missing task information in response")
	}
	return response.Data.TaskID, nil
}

// CreateFAQEntry creates a single FAQ entry synchronously.
func (c *Client) CreateFAQEntry(ctx context.Context,
	knowledgeBaseID string, payload *FAQEntryPayload,
) (*FAQEntry, error) {
	path := fmt.Sprintf("/api/v1/knowledge-bases/%s/faq/entry", knowledgeBaseID)
	resp, err := c.doRequest(ctx, http.MethodPost, path, payload, nil)
	if err != nil {
		return nil, err
	}

	var response FAQEntryResponse
	if err := parseResponse(resp, &response); err != nil {
		return nil, err
	}
	return response.Data, nil
}

// GetFAQEntry retrieves a single FAQ entry by seq_id.
func (c *Client) GetFAQEntry(ctx context.Context,
	knowledgeBaseID string, entrySeqID int64,
) (*FAQEntry, error) {
	path := fmt.Sprintf("/api/v1/knowledge-bases/%s/faq/entries/%d", knowledgeBaseID, entrySeqID)
	resp, err := c.doRequest(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, err
	}

	var response FAQEntryResponse
	if err := parseResponse(resp, &response); err != nil {
		return nil, err
	}
	return response.Data, nil
}

// UpdateFAQEntry updates a single FAQ entry.
func (c *Client) UpdateFAQEntry(ctx context.Context,
	knowledgeBaseID string, entrySeqID int64, payload *FAQEntryPayload,
) (*FAQEntry, error) {
	path := fmt.Sprintf("/api/v1/knowledge-bases/%s/faq/entries/%d", knowledgeBaseID, entrySeqID)
	resp, err := c.doRequest(ctx, http.MethodPut, path, payload, nil)
	if err != nil {
		return nil, err
	}

	var response FAQEntryResponse
	if err := parseResponse(resp, &response); err != nil {
		return nil, err
	}
	return response.Data, nil
}

// AddSimilarQuestionsPayload is used to add similar questions to a FAQ entry.
type AddSimilarQuestionsPayload struct {
	SimilarQuestions []string `json:"similar_questions"`
}

// AddSimilarQuestions adds similar questions to a FAQ entry.
// This will append the new questions to the existing similar questions list.
func (c *Client) AddSimilarQuestions(ctx context.Context,
	knowledgeBaseID string, entrySeqID int64, payload *AddSimilarQuestionsPayload,
) (*FAQEntry, error) {
	path := fmt.Sprintf("/api/v1/knowledge-bases/%s/faq/entries/%d/similar-questions", knowledgeBaseID, entrySeqID)
	resp, err := c.doRequest(ctx, http.MethodPost, path, payload, nil)
	if err != nil {
		return nil, err
	}

	var response FAQEntryResponse
	if err := parseResponse(resp, &response); err != nil {
		return nil, err
	}
	return response.Data, nil
}

// UpdateFAQEntryFieldsBatch updates multiple fields for FAQ entries in bulk.
// Supports updating is_enabled, is_recommended, tag_id in a single call.
// Supports two modes:
//   - byID: update by entry seq_id, key is entry seq_id
//   - byTag: update all entries under a tag, key is tag seq_id (0 for uncategorized)
func (c *Client) UpdateFAQEntryFieldsBatch(ctx context.Context,
	knowledgeBaseID string, byID map[int64]FAQEntryFieldsUpdate, byTag map[int64]FAQEntryFieldsUpdate, excludeIDs []int64,
) error {
	path := fmt.Sprintf("/api/v1/knowledge-bases/%s/faq/entries/fields", knowledgeBaseID)
	resp, err := c.doRequest(ctx, http.MethodPut, path, &FAQEntryFieldsBatchRequest{ByID: byID, ByTag: byTag, ExcludeIDs: excludeIDs}, nil)
	if err != nil {
		return err
	}

	var response faqSimpleResponse
	return parseResponse(resp, &response)
}

// UpdateFAQEntryTagBatch updates FAQ entry tags in bulk.
// key: entry seq_id, value: tag seq_id (nil to remove tag)
func (c *Client) UpdateFAQEntryTagBatch(ctx context.Context,
	knowledgeBaseID string, updates map[int64]*int64,
) error {
	path := fmt.Sprintf("/api/v1/knowledge-bases/%s/faq/entries/tags", knowledgeBaseID)
	resp, err := c.doRequest(ctx, http.MethodPut, path, &FAQEntryTagBatchRequest{Updates: updates}, nil)
	if err != nil {
		return err
	}

	var response faqSimpleResponse
	return parseResponse(resp, &response)
}

// DeleteFAQEntries deletes FAQ entries in bulk by seq_id.
func (c *Client) DeleteFAQEntries(ctx context.Context,
	knowledgeBaseID string, ids []int64,
) error {
	path := fmt.Sprintf("/api/v1/knowledge-bases/%s/faq/entries", knowledgeBaseID)
	resp, err := c.doRequest(ctx, http.MethodDelete, path, &FAQDeleteRequest{IDs: ids}, nil)
	if err != nil {
		return err
	}

	var response faqSimpleResponse
	return parseResponse(resp, &response)
}

// SearchFAQEntries performs hybrid FAQ search inside a knowledge base.
func (c *Client) SearchFAQEntries(ctx context.Context,
	knowledgeBaseID string, payload *FAQSearchRequest,
) ([]FAQEntry, error) {
	path := fmt.Sprintf("/api/v1/knowledge-bases/%s/faq/search", knowledgeBaseID)
	resp, err := c.doRequest(ctx, http.MethodPost, path, payload, nil)
	if err != nil {
		return nil, err
	}

	var response FAQSearchResponse
	if err := parseResponse(resp, &response); err != nil {
		return nil, err
	}

	return response.Data, nil
}

// ExportFAQEntries exports all FAQ entries from a knowledge base as CSV data.
// The CSV format matches the import example format with 8 columns:
// 分类(必填), 问题(必填), 相似问题(选填-多个用##分隔), 反例问题(选填-多个用##分隔),
// 机器人回答(必填-多个用##分隔), 是否全部回复(选填-默认FALSE), 是否停用(选填-默认FALSE),
// 是否禁止被推荐(选填-默认False 可被推荐)
func (c *Client) ExportFAQEntries(ctx context.Context, knowledgeBaseID string) ([]byte, error) {
	return c.exportFAQEntries(ctx, knowledgeBaseID, "")
}

// ExportFAQEntriesJSON exports the same entries as a JSON array in the import
// payload's entry shape, so a snapshot can be edited and re-imported without
// converting a spreadsheet back.
func (c *Client) ExportFAQEntriesJSON(ctx context.Context, knowledgeBaseID string) ([]byte, error) {
	return c.exportFAQEntries(ctx, knowledgeBaseID, "json")
}

func (c *Client) exportFAQEntries(ctx context.Context, knowledgeBaseID, format string) ([]byte, error) {
	path := fmt.Sprintf("/api/v1/knowledge-bases/%s/faq/entries/export", knowledgeBaseID)
	var query url.Values
	if format != "" {
		query = url.Values{"format": {format}}
	}
	resp, err := c.doRequest(ctx, http.MethodGet, path, nil, query)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// The body of a refused export is the server's JSON error, not entries. A
	// caller that saved it would later import the error as CSV/JSON content.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, newAPIErrorFromResponse(resp, readErrorBody(resp.Body))
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read export response: %w", err)
	}

	return data, nil
}

// FAQFailedEntry represents a failed entry during FAQ import/validation.
//
// A partial failure (IsPartialFailure) is not a failed import: the entry lands
// with the questions named in RemovedSimilarQuestions/RemovedNegativeQuestions
// stripped. The two are reported in the same list because both are rows the
// caller has to look at, so a reader that only counts entries must check
// IsPartialFailure to tell them apart -- or read PartialFailedCount, which
// stays correct even after the list has been moved into a CSV.
type FAQFailedEntry struct {
	Index                    int      `json:"index"`
	Reason                   string   `json:"reason"`
	FailureType              string   `json:"failure_type,omitempty"`       // pre_validation / post_validation
	IsPartialFailure         bool     `json:"is_partial_failure,omitempty"` // Imported, but with questions removed
	TagName                  string   `json:"tag_name,omitempty"`
	StandardQuestion         string   `json:"standard_question"`
	SimilarQuestions         []string `json:"similar_questions,omitempty"`
	NegativeQuestions        []string `json:"negative_questions,omitempty"`
	Answers                  []string `json:"answers,omitempty"`
	AnswerAll                bool     `json:"answer_all,omitempty"`
	IsDisabled               bool     `json:"is_disabled,omitempty"`
	RemovedSimilarQuestions  []string `json:"removed_similar_questions,omitempty"`
	RemovedNegativeQuestions []string `json:"removed_negative_questions,omitempty"`
}

// FAQMergeDetail summarises an append-mode entry that merged into an existing
// chunk instead of creating a new one.
type FAQMergeDetail struct {
	Index            int    `json:"index"`
	StandardQuestion string `json:"standard_question"`
	AnswerChanged    bool   `json:"answer_changed"`
	NewSimilarCount  int    `json:"new_similar_count"`
	NewNegativeCount int    `json:"new_negative_count"`
}

// FAQSuccessEntry represents a successfully imported FAQ entry.
type FAQSuccessEntry struct {
	Index            int    `json:"index"`              // Entry index in the batch (0-based)
	SeqID            int64  `json:"seq_id"`             // Entry sequence ID after import
	TagID            int64  `json:"tag_id,omitempty"`   // Tag ID (seq_id)
	TagName          string `json:"tag_name,omitempty"` // Tag name
	StandardQuestion string `json:"standard_question"`  // Standard question
}

// FAQImportProgress represents the progress of an async FAQ import task.
// When Status is "completed", the result fields (SkippedCount, ImportMode, ImportedAt, DisplayStatus, ProcessingTime) are populated.
//
// PartialFailedCount is not optional in practice even though its JSON tag is:
// SuccessCount counts only entries that landed intact, and a partially failed
// entry is in neither it nor FailedCount, so failed_count alone under-reports
// what went wrong. (The server derives SkippedCount as the remainder and floors
// it at 0, so the four counts add up to Total only when nothing else is
// inconsistent -- do not use the identity to recover a missing count.) The
// server also moves the inline FailedEntries list into a CSV (and
// FailedEntriesURL) as soon as there is one, leaving PartialFailedCount as the
// only reliable count after the fact.
//
// Several fields carry omitempty, so a zero value is simply absent from the
// JSON. Absence means zero here, never "this server is too old to have the
// field".
type FAQImportProgress struct {
	TaskID             string            `json:"task_id"`
	KBID               string            `json:"kb_id"`
	KnowledgeID        string            `json:"knowledge_id"`
	Status             string            `json:"status"`
	Progress           int               `json:"progress"`
	Total              int               `json:"total"`
	Processed          int               `json:"processed"`
	SuccessCount       int               `json:"success_count"`
	FailedCount        int               `json:"failed_count"`
	PartialFailedCount int               `json:"partial_failed_count,omitempty"` // Imported, but with conflicting similar/negative questions removed
	SkippedCount       int               `json:"skipped_count,omitempty"`
	FailedEntries      []FAQFailedEntry  `json:"failed_entries,omitempty"`
	SuccessEntries     []FAQSuccessEntry `json:"success_entries,omitempty"`    // Successfully imported entries (when count is small)
	FailedEntriesURL   string            `json:"failed_entries_url,omitempty"` // Object-storage path of the failed-entries CSV; download via the authenticated /files proxy
	// ValidEntryIndices is the set of entries that passed validation. A retry
	// reuses it instead of validating again, which is why it is part of the
	// record rather than an implementation detail.
	ValidEntryIndices []int `json:"valid_entry_indices,omitempty"`
	// MergeEntryIndices is the subset of ValidEntryIndices that merge into an
	// existing chunk in append mode. Internal: read it only to explain a retry.
	MergeEntryIndices []int `json:"merge_entry_indices,omitempty"`
	// NextEntryIndex is the retry cursor into the validated entries. It is not
	// Processed: skip and merge bookkeeping pollutes Processed, so it cannot be
	// used as a slice offset.
	NextEntryIndex int              `json:"next_entry_index,omitempty"`
	MergedCount    int              `json:"merged_count,omitempty"`
	AddedCount     int              `json:"added_count,omitempty"`
	MergeDetails   []FAQMergeDetail `json:"merge_details,omitempty"`
	Message        string           `json:"message"`
	Error          string           `json:"error,omitempty"`
	CreatedAt      int64            `json:"created_at"`
	UpdatedAt      int64            `json:"updated_at"`
	DryRun         bool             `json:"dry_run,omitempty"` // Whether this is a dry run validation

	// Result fields (populated when Status == "completed")
	ImportMode     string    `json:"import_mode,omitempty"`
	ImportedAt     time.Time `json:"imported_at,omitempty"`
	DisplayStatus  string    `json:"display_status,omitempty"`
	ProcessingTime int64     `json:"processing_time,omitempty"`
}

// FAQImportProgressResponse wraps the FAQ import progress response.
type FAQImportProgressResponse struct {
	Success bool               `json:"success"`
	Data    *FAQImportProgress `json:"data"`
	Message string             `json:"message,omitempty"`
	Code    string             `json:"code,omitempty"`
}

// GetFAQImportProgress retrieves the progress of an async FAQ import task.
// This works for both regular imports and dry run validations.
func (c *Client) GetFAQImportProgress(ctx context.Context, taskID string) (*FAQImportProgress, error) {
	path := fmt.Sprintf("/api/v1/faq/import/progress/%s", taskID)
	resp, err := c.doRequest(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, err
	}

	var response FAQImportProgressResponse
	if err := parseResponse(resp, &response); err != nil {
		return nil, err
	}
	return response.Data, nil
}

type updateLastFAQImportResultDisplayStatusRequest struct {
	DisplayStatus string `json:"display_status"`
}

// UpdateLastFAQImportResultDisplayStatus updates the display status (open/close) of the last FAQ import result.
func (c *Client) UpdateLastFAQImportResultDisplayStatus(ctx context.Context, knowledgeBaseID string, displayStatus string) error {
	path := fmt.Sprintf("/api/v1/knowledge-bases/%s/faq/import/last-result/display", knowledgeBaseID)
	resp, err := c.doRequest(ctx, http.MethodPut, path, &updateLastFAQImportResultDisplayStatusRequest{DisplayStatus: displayStatus}, nil)
	if err != nil {
		return err
	}

	var response faqSimpleResponse
	return parseResponse(resp, &response)
}
