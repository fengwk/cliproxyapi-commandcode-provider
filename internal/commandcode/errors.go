package commandcode

// Error codes are stable identifiers the native backend maps to client
// protocol errors. Messages never contain upstream payload or credential
// material; callers must not append raw input to them.
const (
	CodeInvalidRequest      = "invalid_request"
	CodeUnsupported         = "unsupported_feature"
	CodeAccounting          = "accounting_unavailable"
	CodeUsageInvalid        = "usage_invalid"
	CodeUsageConflict       = "usage_conflict"
	CodeStreamTruncated     = "upstream_stream_truncated"
	CodeStreamError         = "upstream_stream_error"
	CodeStreamAborted       = "upstream_aborted"
	CodeMalformedEvent      = "malformed_event"
	CodeContentEncoding     = "unsupported_content_encoding"
	CodeResponseState       = "response_state"
	CodeReasoningEffort     = "unsupported_reasoning_effort"
	CodeContentNotDecodable = "content_not_decodable"
)

// invalidRequest reports a client payload that cannot be transformed losslessly.
func invalidRequest(message string) *Error {
	return &Error{Code: CodeInvalidRequest, Message: message, HTTPStatus: 400}
}

// unsupported reports a client semantic the first version cannot express.
func unsupported(message string) *Error {
	return &Error{Code: CodeUnsupported, Message: message, HTTPStatus: 400}
}

// accountingError reports ambiguous or incomplete upstream usage that cannot be
// priced. The first version fails closed instead of fabricating buckets.
func accountingError(message string) *Error {
	return &Error{Code: CodeAccounting, Message: message, HTTPStatus: 502}
}

func usageInvalid(message string) *Error {
	return &Error{Code: CodeUsageInvalid, Message: message, HTTPStatus: 502}
}

func usageConflict(message string) *Error {
	return &Error{Code: CodeUsageConflict, Message: message, HTTPStatus: 502}
}

func truncatedError() *Error {
	return &Error{Code: CodeStreamTruncated, Message: "upstream stream ended before a terminal event", HTTPStatus: 502, Retryable: true}
}

func malformedEventError() *Error {
	return &Error{Code: CodeMalformedEvent, Message: "upstream stream contained a malformed event", HTTPStatus: 502}
}

func streamError(status int, retryable bool) *Error {
	if status < 400 || status > 599 {
		status = 502
	}
	return &Error{Code: CodeStreamError, Message: "upstream reported a stream error", HTTPStatus: status, Retryable: retryable}
}

func abortedError() *Error {
	return &Error{Code: CodeStreamAborted, Message: "upstream aborted the stream", HTTPStatus: 502}
}

func encodingError(message string) *Error {
	return &Error{Code: CodeContentEncoding, Message: message, HTTPStatus: 502}
}

func responseStateError(message string) *Error {
	return &Error{Code: CodeResponseState, Message: message, HTTPStatus: 500}
}
