package service

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/types/interfaces"
	secutils "github.com/Tencent/WeKnora/internal/utils"
)

// stubFileService serves one fixed object and records whether it was closed.
type stubFileService struct {
	interfaces.FileService
	body   []byte
	closed bool
}

func (s *stubFileService) GetFile(_ context.Context, _ string) (io.ReadCloser, error) {
	return &trackedCloser{Reader: bytes.NewReader(s.body), closed: &s.closed}, nil
}

type trackedCloser struct {
	*bytes.Reader
	closed *bool
}

func (c *trackedCloser) Close() error { *c.closed = true; return nil }

// readFAQEntriesPayload is the worker-side read of an import file. It is the
// one place a payload enters memory without the HTTP handler's byte budget, so
// it has to apply the same cap: a task queued before the budget existed, or a
// retry of one, would otherwise read an arbitrarily large object whole.
func TestReadFAQEntriesPayloadStopsAtTheByteBudget(t *testing.T) {
	t.Setenv(secutils.EnvFAQImportMaxJSONBytes, "1024")

	under := &stubFileService{body: bytes.Repeat([]byte("a"), 1024)}
	data, err := readFAQEntriesPayload(context.Background(), under, "kb/f.json")
	require.NoError(t, err)
	require.Len(t, data, 1024)
	require.True(t, under.closed, "the object has to be closed on the success path")

	over := &stubFileService{body: bytes.Repeat([]byte("a"), 4096)}
	_, err = readFAQEntriesPayload(context.Background(), over, "kb/f.json")
	require.Error(t, err)
	require.Contains(t, err.Error(), "budget")
	require.True(t, over.closed, "the object has to be closed when the read is refused")
}
