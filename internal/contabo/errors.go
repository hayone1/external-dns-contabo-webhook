package contabo

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// ErrInvalidCredentials means Contabo refused the client ID, secret, API user
// or API password. Retrying will not help until the credentials are fixed.
var ErrInvalidCredentials = errors.New("contabo: credentials were rejected")

// APIError is a non-2xx answer from the Contabo API.
type APIError struct {
	Operation  string
	StatusCode int
	Message    string
	RequestID  string
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.StatusCode)
	}
	return fmt.Sprintf("contabo %s: HTTP %d: %s (x-request-id %s)", e.Operation, e.StatusCode, msg, e.RequestID)
}

// Temporary reports whether the same request may succeed later.
func (e *APIError) Temporary() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

// IsNotFound reports whether err is an HTTP 404 from the API.
func IsNotFound(err error) bool { return hasStatus(err, http.StatusNotFound) }

// IsConflict reports whether err is an HTTP 409 from the API. Contabo sends
// 409 when a record with the same name, type and data already exists.
func IsConflict(err error) bool { return hasStatus(err, http.StatusConflict) }

// IsTemporary reports whether err may go away on its own: rate limits,
// server errors and network problems. Invalid input and rejected
// credentials are not temporary.
func IsTemporary(err error) bool {
	if err == nil || errors.Is(err, ErrInvalidCredentials) {
		return false
	}
	if apiErr, ok := errors.AsType[*APIError](err); ok {
		return apiErr.Temporary()
	}
	// Anything else (timeouts, connection resets, DNS failures) is treated
	// as temporary so that external-dns tries again on its next loop.
	return true
}

func hasStatus(err error, code int) bool {
	apiErr, ok := errors.AsType[*APIError](err)
	return ok && apiErr.StatusCode == code
}

// parseErrorMessage pulls a readable message out of an error body. It never
// returns more than 512 bytes so a huge HTML error page cannot flood the logs.
func parseErrorMessage(body []byte) string {
	var parsed apiErrorBody
	if err := json.Unmarshal(body, &parsed); err == nil && len(parsed.Message) > 0 {
		var s string
		if json.Unmarshal(parsed.Message, &s) == nil {
			return truncate(s)
		}
		var list []string
		if json.Unmarshal(parsed.Message, &list) == nil {
			return truncate(strings.Join(list, "; "))
		}
	}
	return truncate(strings.TrimSpace(string(body)))
}

func truncate(s string) string {
	const limit = 512
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}
