package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Tencent/WeKnora/internal/models/vlm"
	"github.com/Tencent/WeKnora/internal/types"
)

// errorVLM fails every prediction, standing in for a model endpoint that
// answered non-2xx: a 429 while the provider throttles, a 5xx, a timeout, or a
// 4xx such as 400/413/431 the endpoint rejected the request with.
type errorVLM struct {
	err error
	// reply stands in for a completion that was cut off: the real client
	// returns the visible prefix together with the truncation error.
	reply string
}

func (m *errorVLM) Predict(_ context.Context, _ [][]byte, _ string) (string, error) {
	return m.reply, m.err
}

func (m *errorVLM) PredictWithOptions(
	_ context.Context, _ [][]byte, _ string, _ *vlm.PredictOptions,
) (string, error) {
	return m.reply, m.err
}

func (m *errorVLM) GetModelName() string { return "error" }
func (m *errorVLM) GetModelID() string   { return "error" }

var _ vlm.VLM = (*errorVLM)(nil)

// emptyVLM answers every prediction with no text: the call succeeded, but the
// output carries nothing readable.
type emptyVLM struct{}

func (emptyVLM) Predict(_ context.Context, _ [][]byte, _ string) (string, error) {
	return "", nil
}

func (emptyVLM) PredictWithOptions(
	_ context.Context, _ [][]byte, _ string, _ *vlm.PredictOptions,
) (string, error) {
	return "", nil
}

func (emptyVLM) GetModelName() string { return "empty" }
func (emptyVLM) GetModelID() string   { return "empty" }

var _ vlm.VLM = emptyVLM{}

func ocrRunContext(model vlm.VLM) *runContext {
	return &runContext{
		payload:    &types.ImageMultimodalPayload{ImageSourceType: "image"},
		model:      model,
		imageBytes: []byte("fake image bytes"),
		vlmCfg:     types.VLMConfig{},
		imageInfo:  &types.ImageInfo{},
		out:        types.JSONMap{},
	}
}

// TestRunOCRActionRecordsFailureWithoutFailing pins the #4132 contract: a
// failed OCR call is RECORDED (status, stable error code, raw message) but
// must NOT fail the action — retrying the whole image would re-run a caption
// that already succeeded and duplicate its chunk. The failure stays visible
// through the recorded fields and the outcome summary, never through a
// swallowed empty result.
func TestRunOCRActionRecordsFailureWithoutFailing(t *testing.T) {
	r := ocrRunContext(&errorVLM{err: errors.New("429 too many requests")})

	if err := runOCRAction(context.Background(), r); err != nil {
		t.Fatalf("runOCRAction error = %v, want nil (failure is recorded, not propagated)", err)
	}
	if got := r.out[keyOCRResult]; got != "failed" {
		t.Errorf("ocr_status = %v, want failed", got)
	}
	if got := r.out[keyOCRErrorCode]; got != "OCR_REQUEST_FAILED" {
		t.Errorf("ocr_error_code = %v, want OCR_REQUEST_FAILED", got)
	}
	if _, ok := r.out[keyOCRError]; !ok {
		t.Errorf("ocr_error not recorded for a failed call")
	}
	if got := r.out[keyOCRResult]; got != "failed" {
		t.Errorf("a failed call must read as failed, never as skipped: %v", got)
	}
	if got := r.imageInfo.OCRText; got != "" {
		t.Errorf("a failed call must leave no OCR text behind, got %q", got)
	}
}

// TestRunOCRActionCodesTruncation pins the budget-exhaustion branch: an error
// wrapping vlm.ErrTruncatedCompletion is re-coded OCR_TRUNCATED so the trace
// distinguishes "the model ran out of budget" from "the request failed". With
// no usable prefix on the error there is nothing to index and the image fails.
func TestRunOCRActionCodesTruncation(t *testing.T) {
	r := ocrRunContext(&errorVLM{err: &vlm.TruncatedCompletionError{MaxTokens: 16000}})

	if err := runOCRAction(context.Background(), r); err != nil {
		t.Fatalf("runOCRAction error = %v, want nil", err)
	}
	if got := r.out[keyOCRErrorCode]; got != "OCR_TRUNCATED" {
		t.Errorf("ocr_error_code = %v, want OCR_TRUNCATED", got)
	}
}

// TestRunOCRActionRejectsEmptyOutput pins the validation half: a successful
// call whose output has nothing readable (empty, or a known-empty reply the
// sanitizer does not whitelist) is an INVALID output — failed with
// OCR_INVALID_OUTPUT — and not a success. A model that explicitly answers "no
// text" is the no_text branch the validation suite covers.
func TestRunOCRActionRejectsEmptyOutput(t *testing.T) {
	r := ocrRunContext(emptyVLM{})

	if err := runOCRAction(context.Background(), r); err != nil {
		t.Fatalf("runOCRAction returned %v for an empty-but-successful answer", err)
	}
	if got := r.out[keyOCRResult]; got != "failed" {
		t.Errorf("ocr_status = %v, want failed (empty output is invalid, not no_text)", got)
	}
	if got := r.out[keyOCRErrorCode]; got != "OCR_INVALID_OUTPUT" {
		t.Errorf("ocr_error_code = %v, want OCR_INVALID_OUTPUT", got)
	}
	if _, ok := r.out[keyOCRError]; !ok {
		t.Errorf("ocr_error not recorded for invalid output")
	}
}

// TestDefaultPipelineRecordsOCRFailure is the #4064 regression at the level it
// was reported: the manual pipeline used to swallow the OCR failure, so
// processImage found neither caption nor text and finished the image as done.
// The failure now surfaces as recorded fields (and a failed .ocr subspan in
// the full processImage path) while pipeline.Run itself stays green — the
// outcome summary, not a task retry, is what carries the signal.
func TestDefaultPipelineRecordsOCRFailure(t *testing.T) {
	payload := &types.ImageMultimodalPayload{
		ImageAttrsEnabled: false,
		ImagePipelineID:   types.ImagePipelineDefault,
	}
	pipeline := selectImagePipeline(payload)
	if pipeline == nil {
		t.Fatal("default pipeline not registered")
	}
	r := &runContext{
		payload:    payload,
		declared:   pipeline.Fields(),
		model:      &errorVLM{err: errors.New("503 service unavailable")},
		imageBytes: []byte("fake image bytes"),
		vlmCfg:     types.VLMConfig{},
		imageInfo:  &types.ImageInfo{},
		out:        types.JSONMap{},
	}

	if err := pipeline.Run(context.Background(), r); err != nil {
		t.Fatalf("pipeline.Run error = %v, want nil (failure is recorded, not propagated)", err)
	}
	if got := r.out[keyOCRResult]; got != "failed" {
		t.Errorf("ocr_status = %v, want failed", got)
	}
	if got := r.out[keyOCRError]; got == "" {
		t.Error("ocr_error not recorded for a failed call")
	}
}

// TestRunOCRActionKeepsTruncatedText covers the case the production logs
// showed: a dense page stopped at the budget after transcribing most of it.
// Dropping the page loses every setting it listed, so the readable prefix is
// indexed and only the incompleteness is recorded.
func TestRunOCRActionKeepsTruncatedText(t *testing.T) {
	partial := "1）初始化\n2）WLAN设置\n3）网络配置 172.16.0.254/24"
	r := ocrRunContext(&errorVLM{err: &vlm.TruncatedCompletionError{MaxTokens: 16000, Partial: partial},
		reply: partial})

	if err := runOCRAction(context.Background(), r); err != nil {
		t.Fatalf("runOCRAction error = %v, want nil", err)
	}
	if got := r.out[keyOCRResult]; got != resultTruncated {
		t.Errorf("ocr_status = %v, want %s", got, resultTruncated)
	}
	if r.imageInfo.OCRText != partial {
		t.Errorf("ocr text = %q, want the transcribed prefix kept", r.imageInfo.OCRText)
	}
	if got := r.out[keyOCRPartial]; got != true {
		t.Errorf("ocr.partial = %v, want true", got)
	}
	if got := r.out[keyOCRChars]; got != len([]rune(partial)) {
		t.Errorf("ocr.chars = %v, want %d", got, len([]rune(partial)))
	}
	if _, ok := r.out[keyOCRError]; ok {
		t.Errorf("a kept transcription must not be reported as an error: %v", r.out[keyOCRError])
	}
}
