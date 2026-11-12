package api

import "context"

// transportRetryDisabledKey marks a context whose caller already retries the
// stream opening handshake itself.
type transportRetryDisabledKey struct{}

// WithoutTransportRetry tells a chat implementation that retries its opening
// handshake internally to skip that retry for this call. Callers that wrap the
// call in their own retry loop use it to keep the two budgets from multiplying
// (a pipeline retry of 3 stacked on a transport retry of 3 issues up to 9
// handshakes for one user request).
func WithoutTransportRetry(ctx context.Context) context.Context {
	return context.WithValue(ctx, transportRetryDisabledKey{}, true)
}

// TransportRetryDisabled reports whether the caller retries the handshake
// itself and the transport-level retry must therefore be skipped.
func TransportRetryDisabled(ctx context.Context) bool {
	disabled, _ := ctx.Value(transportRetryDisabledKey{}).(bool)
	return disabled
}
