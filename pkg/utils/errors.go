package utils

import "strings"

// CategorizeError categorizes errors for metrics tracking
// Returns a string representing the error type for use in metrics labels
func CategorizeError(err error) string {
	if err == nil {
		return "none"
	}

	errStr := err.Error()

	// Check for common error patterns
	switch {
	case strings.Contains(errStr, "connection refused"):
		return "connection_refused"
	case strings.Contains(errStr, "timeout"):
		return "timeout"
	case strings.Contains(errStr, "no such host"):
		return "dns_error"
	case strings.Contains(errStr, "context canceled"):
		return "context_canceled"
	case strings.Contains(errStr, "context deadline exceeded"):
		return "context_deadline"
	case strings.Contains(errStr, "failed to connect"):
		return "connection_failed"
	case strings.Contains(errStr, "failed to get block number"):
		return "rpc_error"
	case strings.Contains(errStr, "unexpected status code"):
		return "http_error"
	case strings.Contains(errStr, "failed to send request"):
		return "request_failed"
	case strings.Contains(errStr, "failed to read response"):
		return "response_read_failed"
	case strings.Contains(errStr, "failed to marshal"):
		return "marshal_error"
	case strings.Contains(errStr, "failed to create request"):
		return "request_creation_failed"
	default:
		return "unknown"
	}
}
