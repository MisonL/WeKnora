package utils

import (
	"log"
	"os"
	"strconv"
	"strings"
)

// Document upload admission knobs.
//
// Document uploads are the one path that both receives large bodies and leaves
// queued work behind, and until these knobs existed the only bound was
// MAX_FILE_SIZE_MB per request. The handler side uses the first three (slots and
// in-flight bytes); the parse-job side uses the fourth (see
// service.ReserveUploadWork).
const (
	// EnvUploadMaxConcurrency is the number of document uploads admitted at once.
	EnvUploadMaxConcurrency = "UPLOAD_MAX_CONCURRENCY"
	// EnvUploadBigBytes is the declared size at or above which an upload counts
	// as big. Only one big upload is admitted at a time regardless of the total.
	EnvUploadBigBytes = "UPLOAD_BIG_BYTES"
	// EnvUploadMaxInFlightBytes caps the declared bytes of in-flight uploads.
	EnvUploadMaxInFlightBytes = "UPLOAD_MAX_INFLIGHT_BYTES"
	// EnvUploadMaxBackgroundBytes caps the declared bytes of parse jobs that are
	// queued or running. Held by the job, not by the request.
	EnvUploadMaxBackgroundBytes = "UPLOAD_MAX_BACKGROUND_BYTES"
	// EnvUploadMaxBackgroundJobs caps how many parse jobs are queued or running.
	// The byte budget alone does not bound small documents, and a job whose
	// input size is unknown still costs a queue slot and a reservation.
	EnvUploadMaxBackgroundJobs = "UPLOAD_MAX_BACKGROUND_JOBS"
)

const (
	DefaultUploadMaxConcurrency     = 2
	DefaultUploadBigBytes           = int64(32 << 20)
	DefaultUploadMaxInFlightBytes   = int64(128 << 20)
	DefaultUploadMaxBackgroundBytes = int64(512 << 20)
	DefaultUploadMaxBackgroundJobs  = 1000
)

// GetUploadMaxConcurrency returns how many document uploads may be in flight.
func GetUploadMaxConcurrency() int {
	n := envPositiveInt(EnvUploadMaxConcurrency, DefaultUploadMaxConcurrency)
	return int(n)
}

// GetUploadBigBytes returns the declared size at or above which an upload counts
// as big.
func GetUploadBigBytes() int64 { return envPositiveInt(EnvUploadBigBytes, DefaultUploadBigBytes) }

// GetUploadMaxInFlightBytes returns the declared-byte budget for uploads whose
// body is still being read.
func GetUploadMaxInFlightBytes() int64 {
	return envPositiveInt(EnvUploadMaxInFlightBytes, DefaultUploadMaxInFlightBytes)
}

// GetUploadMaxBackgroundBytes returns the declared-byte budget for parse jobs
// queued or running.
func GetUploadMaxBackgroundBytes() int64 {
	return envPositiveInt(EnvUploadMaxBackgroundBytes, DefaultUploadMaxBackgroundBytes)
}

// GetUploadMaxBackgroundJobs returns how many parse jobs may be queued or
// running at once.
func GetUploadMaxBackgroundJobs() int64 {
	return envPositiveInt(EnvUploadMaxBackgroundJobs, DefaultUploadMaxBackgroundJobs)
}

// envPositiveInt reads a positive integer knob and falls back to def when the
// value is absent or unusable. A mistyped value is logged: silently falling back
// would leave an operator with a limit they believe they changed.
func envPositiveInt(key string, def int64) int64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		log.Printf("[config] %s=%q is not a positive integer; using the default of %d", key, v, def)
		return def
	}
	return n
}
