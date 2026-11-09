package openaicompletions

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/models/api"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

// cancelAfterReader serves a scripted body and then reports context.Canceled
// instead of io.EOF, reproducing what an HTTP response body does when the
// consumer cancels the request after the vendor already finished the message.
type cancelAfterReader struct{ r io.Reader }

func (c *cancelAfterReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if err == io.EOF {
		return n, context.Canceled
	}
	return n, err
}

// The IM reply path cancels its context as soon as it sees the natural stop,
// which used to arrive one read later as context.Canceled and was reported as
// "Stream read error" — one ERROR line per answer, with the usage record of the
// answer dropped because Fail never logs it.
func TestProcessStreamTreatsCancellationAfterFinishAsCleanEnd(t *testing.T) {
	var body strings.Builder
	body.WriteString(`data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n")
	body.WriteString(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n")

	client := newClient(t, nil)
	ch := make(chan types.StreamResponse, 64)
	client.processStream(context.Background(),
		&cancelAfterReader{r: strings.NewReader(body.String())}, ch, nil, &api.Options{})
	close(ch)

	var sawError bool
	var content, finishReason strings.Builder
	for resp := range ch {
		if resp.ResponseType == types.ResponseTypeError {
			sawError = true
		}
		if resp.ResponseType != types.ResponseTypeAnswer {
			continue
		}
		if resp.Done {
			// The assembler closes the message when the finish_reason arrives
			// and again at End; the last one carries the final state.
			finishReason.Reset()
			finishReason.WriteString(resp.FinishReason)
			continue
		}
		content.WriteString(resp.Content)
	}

	require.False(t, sawError, "a stream that already reported finish_reason must not surface an error")
	require.Equal(t, "stop", finishReason.String())
	require.Equal(t, "hi", content.String())
}

// The counterpart guard: cancellation without a finish_reason is a truncated
// answer and must still fail loudly.
func TestProcessStreamReportsCancellationWithoutFinish(t *testing.T) {
	var body strings.Builder
	body.WriteString(`data: {"choices":[{"index":0,"delta":{"content":"partial"}}]}` + "\n\n")

	client := newClient(t, nil)
	ch := make(chan types.StreamResponse, 64)
	client.processStream(context.Background(),
		&cancelAfterReader{r: strings.NewReader(body.String())}, ch, nil, &api.Options{})
	close(ch)

	var sawError bool
	for resp := range ch {
		if resp.ResponseType == types.ResponseTypeError {
			sawError = true
		}
	}
	require.True(t, sawError, "a stream cancelled before finishing must still report an error")
}
