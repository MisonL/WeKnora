package main

import (
	"net/http"
	"time"
)

// newHTTPServer is the one place the server's connection timeouts are set.
//
// ReadHeaderTimeout bounds the request line and headers, so a client that
// dribbles them out byte by byte cannot hold a handler slot. The request body
// is bounded separately, per request, by middleware.RequestBodyDeadline: a
// server-wide ReadTimeout here would leave a deadline armed for the lifetime
// of a long-lived SSE stream and cancel it. The server has no WriteTimeout on
// purpose: those same endpoints stream for as long as the answer takes.
func newHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}
