package demo

import (
	"context"
	"net/http"
	"time"
)

// contextWithTimeout bounds a handler's work without discarding the request's
// own cancellation, so a client that hangs up still unblocks the handler.
func contextWithTimeout(r *http.Request, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), timeout)
}
