package handler

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/errors"
)

// TestExportAdmissionKeepsOneExportAtATime pins the gate: a second concurrent
// export is refused as retryable, and the slot frees when the first ends.
func TestExportAdmissionKeepsOneExportAtATime(t *testing.T) {
	faqExportInflight.Store(0)
	t.Cleanup(func() { faqExportInflight.Store(0) })

	err, release := admitFAQExport()
	require.Nil(t, err)
	require.NotNil(t, release)

	busyErr, busyRelease := admitFAQExport()
	require.NotNil(t, busyErr, "a second export must be refused while one is running")
	require.Equal(t, errors.ErrTooManyRequests, busyErr.Code)
	require.Greater(t, busyErr.RetryAfterSeconds, 0, "a refusal has to tell the caller when to come back")
	require.Nil(t, busyRelease)

	release()
	err, release = admitFAQExport()
	require.Nil(t, err, "the slot frees when the previous export ends")
	release()

	require.Equal(t, int64(0), faqExportInflight.Load())
}
