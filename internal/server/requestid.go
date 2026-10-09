package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

// headerRequestID is the response header carrying the per-request identifier.
// Echoing it lets a client (or the platform proxy) correlate a response with
// the server's audit log line.
const headerRequestID = "X-Request-Id"

// requestIDKey is the context key under which the request identifier is stored.
type requestIDKey struct{}

// fallbackRequestID makes the identifier unique if the system random source is
// ever unavailable; it is not used on the normal path.
var fallbackRequestID atomic.Uint64

// newRequestID returns a fresh, unguessable request identifier: 16 random bytes
// rendered as hex, with a time+counter fallback if crypto/rand fails.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return fmt.Sprintf("%x-%x", time.Now().UnixNano(), fallbackRequestID.Add(1))
}

// requestIDFrom returns the request identifier stored on ctx, or "" when absent.
func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// withRequestID assigns every request a fresh identifier, exposes it on the
// request context for the audit log, and echoes it in the X-Request-Id response
// header. It wraps the whole handler set, so the header is present on every
// response, including refusals.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		w.Header().Set(headerRequestID, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}
