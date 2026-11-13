package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/models/vlm"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

type ocrTestTracker struct {
	noopSpanTracker
	failed  map[string]string
	outputs map[string]types.JSONMap
}

func (t *ocrTestTracker) LookupStage(context.Context, string, int, string) *Span {
	return &Span{Name: "multimodal"}
}

func (t *ocrTestTracker) BeginSubSpan(_ context.Context, _ *Span, name, _ string, _ types.JSONMap) *Span {
	return &Span{Name: name}
}

func (t *ocrTestTracker) FailSpan(_ context.Context, s *Span, code, _ string, _ error) {
	t.failed[s.Name] = code
}

func (t *ocrTestTracker) EndSpan(_ context.Context, s *Span, out types.JSONMap) {
	t.outputs[s.Name] = out
}

func TestImageOCRRejectsInvalidContentBeforePersistence(t *testing.T) {
	for _, tc := range []struct {
		name, raw, status, code string
		err                     error
		// partialErr makes the fake fail the way the real client does: the
		// visible prefix rides on the error instead of being dropped.
		partialErr bool
	}{
		{
			// A page cut off by the budget still carries real text. The
			// transcribed part is kept so the page stays searchable; the
			// truncation stays visible as its own status.
			name: "truncated with readable text", raw: "1）初始化 注意 A：除秤号、网络设置外其余数据恢复出厂默认",
			status: resultTruncated, code: "OCR_TRUNCATED",
			err: &vlm.TruncatedCompletionError{MaxTokens: 5000, Partial: "1）初始化 注意 A：除秤号、网络设置外其余数据恢复出厂默认"},
		},
		{
			// The same budget cut over a runaway fill of empty table cells
			// yields nothing usable, so the page is still a failure.
			name: "truncated with filler only", raw: strings.Repeat("|  ", 2500),
			status: "failed", code: "OCR_TRUNCATED", partialErr: true,
		},
		{name: "recorded empty cells", raw: strings.Repeat("|  ", 2500), status: "failed", code: "OCR_INVALID_OUTPUT"},
		{name: "repeated words", raw: strings.Repeat("传球按钮 ", 200), status: "failed", code: "OCR_INVALID_OUTPUT"},
		{
			name: "split delimiter loops", raw: strings.Repeat("|  ", 1250) + "传球\n" + strings.Repeat("|  ", 1250),
			status: "failed", code: "OCR_INVALID_OUTPUT",
		},
		{name: "empty model output", status: "failed", code: "OCR_INVALID_OUTPUT"},
		{
			name: "truncated output", status: "failed", code: "OCR_TRUNCATED",
			err: &vlm.TruncatedCompletionError{MaxTokens: 16000},
		},
		{name: "request failure", status: "failed", code: "OCR_REQUEST_FAILED", err: errors.New("model unavailable")},
		{name: "no text", raw: "No text content.", status: "skipped_no_text"},
		{name: "HTML no text", raw: "<p>No text content.</p>", status: "skipped_no_text"},
		{name: "HTML short no text", raw: "<p>empty</p>", status: "skipped_no_text"},
		{name: "HTML emphasized no text", raw: "<p><strong>No text content.</strong></p>", status: "skipped_no_text"},
		{name: "empty HTML", raw: "<html><body></body></html>", status: "failed", code: "OCR_INVALID_OUTPUT"},
	} {
		for _, caption := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/caption=%v", tc.name, caption), func(t *testing.T) {
				repo := &attrsChunkRepo{}
				svc := newAttrsTestService(&attrsFileService{body: testPNG(t)}, repo)
				model := &attrsFakeVLM{reply: func(prompt string, _ int) (string, error) {
					if prompt == vlmOCRPrompt {
						if tc.partialErr {
							return tc.raw, &vlm.TruncatedCompletionError{MaxTokens: 5000, Partial: tc.raw}
						}
						return tc.raw, tc.err
					}
					return "A game screenshot.", nil
				}}
				tracker := &ocrTestTracker{failed: map[string]string{}, outputs: map[string]types.JSONMap{}}
				out := types.JSONMap{}
				// The pipeline framework carries the caption/OCR switches as
				// the manual pipeline's own tunables (both default true), the
				// whole-task EnableOCR/EnableCaption pair of the action loop
				// having been superseded by the image_pipeline field.
				err := svc.processImage(context.Background(), &types.ImageMultimodalPayload{
					ImageURL: "local://test.png",
					ImagePipelineParams: map[string]any{
						manualCaptionKey: caption,
						manualOCRKey:     true,
					},
					Attempt: 1,
				}, model, types.VLMConfig{}, tracker, out)
				if caption || tc.status == resultTruncated {
					// A valid caption exists, or the cut page still yielded
					// usable text: the incompleteness is recorded, not
					// propagated — failing the image would retry it and
					// duplicate the caption chunk (#4132).
					require.NoError(t, err)
				} else if tc.code != "" {
					// Caption off + OCR failed = every VLM call failed and
					// nothing was produced: the image fails honestly instead
					// of passing as a silent skip (there is no caption to
					// protect, so the #4132 no-duplicate rationale is void).
					require.Error(t, err, "bad OCR with no caption must fail the image")
				} else {
					// OCR answered no_text: a genuine blank, not a failure.
					require.NoError(t, err)
				}
				require.Equal(t, tc.status, out[keyOCRResult])
				if tc.status == resultTruncated {
					// Keep the span marked incomplete so operators can see
					// the page was cut, but the transcribed part is indexed.
					require.Equal(t, "OCR_TRUNCATED", tracker.failed["multimodal.image[0].ocr"])
					require.NotEmpty(t, out[keyOCRChars], "kept OCR text must be counted")
					require.Contains(t, out, keyOCRPreview)
					require.Equal(t, true, out[keyOCRPartial])
					require.NotContains(t, out, keyOCRError)
					// The image carries usable content either way.
					require.NoError(t, err)
					require.Equal(t, "done", out["outcome"])
					return
				}
				require.NotContains(t, out, keyOCRPartial)
				require.Equal(t, 0, out[keyOCRChars])
				require.NotContains(t, out, "ocr_preview")
				require.Equal(t, tc.code, tracker.failed["multimodal.image[0].ocr"])
				if tc.code != "" {
					require.Equal(t, tc.code, out[keyOCRErrorCode])
					require.NotEmpty(t, out[keyOCRError])
					require.NotContains(t, tracker.outputs, "multimodal.image[0].ocr",
						"failed OCR must not be marked done")
					if caption {
						require.Equal(t, "partial_failure", out["outcome"])
					} else {
						require.Equal(t, "failed", out["outcome"])
					}
				} else {
					require.Empty(t, tracker.failed)
					require.NotContains(t, out, keyOCRError)
					require.NotContains(t, out, keyOCRErrorCode)
					// Every image states its end state now, so "no text" is a
					// verdict rather than an absent key: nothing was extracted
					// when there was no caption to keep, and the run is a plain
					// success when the caption is what survived.
					if caption {
						require.Equal(t, "done", out["outcome"])
					} else {
						require.Equal(t, "skipped_no_content", out["outcome"])
					}
					require.Equal(t, "skipped_no_text", tracker.outputs["multimodal.image[0].ocr"]["status"])
				}
				if caption {
					require.Len(t, repo.created, 1)
				} else {
					require.Empty(t, repo.created)
				}
				for _, chunk := range repo.created {
					require.Equal(t, types.ChunkTypeImageCaption, chunk.ChunkType)
					require.NotContains(t, chunk.ImageInfo, "|  |")
					require.NotContains(t, chunk.ImageInfo, "传球按钮")
				}
			})
		}
	}
}
