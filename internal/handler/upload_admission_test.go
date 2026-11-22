package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func withUploadLimits(t *testing.T, concurrency int, bigBytes, maxBytes int64) {
	t.Helper()
	t.Setenv(UploadMaxConcurrencyEnv, itoaInt(concurrency))
	t.Setenv(UploadBigBytesEnv, itoaInt64(bigBytes))
	t.Setenv(UploadMaxInFlightBytesEnv, itoaInt64(maxBytes))
	resetUploadAdmission()
	t.Cleanup(resetUploadAdmission)
}

func itoaInt(n int) string { return itoaInt64(int64(n)) }
func itoaInt64(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// The byte budget is what keeps two admitted slots from both being
// maximum-size bodies: each fits alone, together they exceed the cap.
func TestAdmitUploadRefusesWhenDeclaredBytesExceedTheBudget(t *testing.T) {
	withUploadLimits(t, 4, 1<<30, 100)

	first, err := admitUpload(60)
	require.NoError(t, err)

	_, err = admitUpload(60)
	require.ErrorIs(t, err, errUploadBusy)

	first.release()
	second, err := admitUpload(60)
	require.NoError(t, err, "the released bytes must be reusable")
	second.release()
}

// Only one big upload at a time, even when the concurrency budget allows more
// and the bytes would fit.
func TestAdmitUploadAllowsOneBigUpload(t *testing.T) {
	withUploadLimits(t, 4, 32, 1<<30)

	big, err := admitUpload(32)
	require.NoError(t, err)

	_, err = admitUpload(32)
	require.ErrorIs(t, err, errUploadBusy)

	small, err := admitUpload(1)
	require.NoError(t, err, "a small upload is not blocked by a running big one")

	small.release()
	big.release()
	again, err := admitUpload(40)
	require.NoError(t, err)
	again.release()
}

// The slot count itself is a ceiling.
func TestAdmitUploadRefusesBeyondTheSlotCount(t *testing.T) {
	withUploadLimits(t, 2, 1<<30, 1<<30)

	a, err := admitUpload(1)
	require.NoError(t, err)
	b, err := admitUpload(1)
	require.NoError(t, err)
	_, err = admitUpload(1)
	require.ErrorIs(t, err, errUploadBusy)

	a.release()
	c, err := admitUpload(1)
	require.NoError(t, err)
	b.release()
	c.release()

	require.Equal(t, int64(0), uploadInflight.Load())
	require.Equal(t, int64(0), uploadInflightBytes.Load())
	require.Equal(t, int64(0), uploadBigInflight.Load())
}

// A deferred release plus an early-return release must not double-subtract: a
// negative counter would silently disable the cap.
func TestUploadSlotReleaseIsIdempotent(t *testing.T) {
	withUploadLimits(t, 2, 1<<30, 1<<30)

	slot, err := admitUpload(10)
	require.NoError(t, err)
	slot.release()
	slot.release()

	require.Equal(t, int64(0), uploadInflight.Load())
	require.Equal(t, int64(0), uploadInflightBytes.Load())
}

// The admission decision is made before the body is read, so it can only use
// Content-Length; a missing or oversized length has to resolve to the cap (the
// conservative side), never to zero.
func TestUploadDeclaredBytesFallsBackToTheCap(t *testing.T) {
	gin.SetMode(gin.TestMode)
	makeCtx := func(contentLength int64) *gin.Context {
		c, _ := gin.CreateTestContext(nil)
		req := httptest.NewRequest(http.MethodPost, "/upload", nil)
		// A negative ContentLength is how net/http marks "unknown" for a
		// chunked body; 0 is a genuinely empty one.
		req.ContentLength = contentLength
		c.Request = req
		return c
	}

	const capBytes = int64(50 << 20)
	require.Equal(t, int64(1024), uploadDeclaredBytes(makeCtx(1024+uploadEnvelopeSlack), capBytes))
	require.Equal(t, capBytes, uploadDeclaredBytes(makeCtx(-1), capBytes), "no length means unknown")
	require.Equal(t, capBytes, uploadDeclaredBytes(makeCtx(0), capBytes), "no length means unknown")
	require.Equal(t, capBytes, uploadDeclaredBytes(makeCtx(capBytes*2), capBytes), "a longer body cannot cost more than the cap")
	// A file at exactly the cap is a legal maximum-size upload, not a bigger one.
	require.Equal(t, capBytes, uploadDeclaredBytes(makeCtx(capBytes+uploadEnvelopeSlack), capBytes))
}
