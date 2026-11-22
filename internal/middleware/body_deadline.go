package middleware

import (
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	// bodyReadBaseGrace is how long a client has to send anything at all
	// before the floor rate starts to matter.
	bodyReadBaseGrace = time.Minute
	// bodyReadFloorRate is the slowest a client may average while sending a
	// body, in bytes per second. It bounds the "send one byte every minute"
	// client that keeps a handler and an upload slot forever without ever
	// tripping an idle timeout.
	bodyReadFloorRate = 32 * 1024
	// bodyReadCap is the most any body may take, whatever its declared size.
	bodyReadCap = 30 * time.Minute
)

// RequestBodyDeadline puts a deadline on reading each request body, so a
// client that never finishes sending it cannot hold the handler — and the
// upload admission slot it may own — indefinitely. The deadline is derived
// from the declared Content-Length at a floor rate, so a legitimately slow
// upload of a big file still fits, while a stalled one does not.
//
// This is per-request and covers only the body read, unlike http.Server's
// ReadTimeout: that one leaves a deadline armed for the whole connection,
// which cancels the request context of a long-lived SSE stream once it
// expires (a GET has no body, so the server keeps a background read pending
// to notice the client hanging up). Here the deadline is cleared as soon as
// the body is consumed, which for the streaming chat POST happens before the
// stream starts.
func RequestBodyDeadline() gin.HandlerFunc {
	return requestBodyDeadlineWith(bodyReadBudget)
}

// requestBodyDeadlineWith takes the budget function so tests can use a
// sub-second deadline instead of waiting for the real one.
func requestBodyDeadlineWith(budget func(declaredBytes int64) time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		body := c.Request.Body
		if body == nil || body == http.NoBody {
			c.Next()
			return
		}
		rc := http.NewResponseController(c.Writer)
		if err := rc.SetReadDeadline(time.Now().Add(budget(c.Request.ContentLength))); err != nil {
			// HTTP/2 and other writers that cannot set a read deadline: the
			// fronting proxy's client_body_timeout is the remaining bound.
			c.Next()
			return
		}
		cleared := false
		clear := func() {
			if cleared {
				return
			}
			cleared = true
			_ = rc.SetReadDeadline(time.Time{})
		}
		c.Request.Body = &deadlineBody{ReadCloser: body, clear: clear}
		// A handler may return without reading the body to EOF (or without
		// reading it at all); the deadline must not outlive the request, or
		// the next request on this keep-alive connection would inherit it.
		defer clear()
		c.Next()
	}
}

// bodyReadBudget is how long a body of declaredBytes may take.
func bodyReadBudget(declaredBytes int64) time.Duration {
	if declaredBytes <= 0 {
		// Chunked or unknown length: only the cap applies.
		return bodyReadCap
	}
	// Clamp before multiplying: declaredBytes is attacker-controlled, and
	// seconds*time.Second overflows int64 into a negative budget, which would
	// expire the deadline the moment it is set.
	seconds := declaredBytes / bodyReadFloorRate
	if seconds >= int64(bodyReadCap/time.Second) {
		return bodyReadCap
	}
	return bodyReadBaseGrace + time.Duration(seconds)*time.Second
}

// deadlineBody clears the read deadline once the body reaches its end, so a
// handler that consumes the body first and then runs long (streaming a chat
// answer, importing entries) is not bounded by the read budget.
type deadlineBody struct {
	io.ReadCloser
	clear func()
}

func (b *deadlineBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.clear()
	}
	return n, err
}
