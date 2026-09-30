package cc

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// ErrorDetail / ErrorBody mirror the wire error shapes the reference proxy
// emits ({"error":{message,type,code?},"retry_after"?}).
type ErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

type ErrorBody struct {
	Error      ErrorDetail `json:"error"`
	RetryAfter int         `json:"retry_after,omitempty"`
}

// MappedError is the result of translating an upstream failure to a
// downstream response. Port of mapCcError / mapCcEventError.
type MappedError struct {
	Status         int
	Code           string // machine-readable upstream classification (BAD_REQUEST, USAGE_EXCEEDED, …)
	ReportedStatus int    // status carried by an in-stream error event (0 = none)
	Body           ErrorBody
}

func (m *MappedError) Error() string { return m.Body.Error.Message }

// statusTarget is the downstream (status, error.type) for an upstream status.
type statusTarget struct {
	Status int
	Type   string
}

// CCStatusMap — port of CC_STATUS_MAP. Unlisted statuses become 502 upstream_error.
// 402 (payment required) is deliberately treated as a rate limit.
var CCStatusMap = map[int]statusTarget{
	400: {400, "invalid_request_error"},
	401: {401, "authentication_error"},
	402: {429, "rate_limit_error"},
	403: {401, "authentication_error"},
	404: {404, "not_found"},
	422: {400, "invalid_request_error"},
	429: {429, "rate_limit_error"},
	500: {502, "upstream_error"},
	502: {502, "upstream_error"},
	503: {503, "temporarily_unavailable"},
}

func mapStatus(ccStatus int) statusTarget {
	if t, ok := CCStatusMap[ccStatus]; ok {
		return t
	}
	return statusTarget{502, "upstream_error"}
}

type upstreamErrorPayload struct {
	Error *struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	} `json:"error"`
	Message string `json:"message"`
	Code    string `json:"code"`
}

// MapCcError translates a non-2xx upstream HTTP response.
func MapCcError(ccStatus int, ccBody string) *MappedError {
	mapped := mapStatus(ccStatus)
	message := fmt.Sprintf("CC API error (%d)", ccStatus)
	code := ""

	if ccBody != "" {
		var parsed upstreamErrorPayload
		if err := json.Unmarshal([]byte(ccBody), &parsed); err == nil {
			if parsed.Error != nil {
				if parsed.Error.Message != "" {
					message = parsed.Error.Message
				}
				code = parsed.Error.Code
			}
			if message == "" || message == fmt.Sprintf("CC API error (%d)", ccStatus) {
				if parsed.Message != "" {
					message = parsed.Message
				}
			}
			if code == "" {
				code = parsed.Code
			}
		} else {
			if len(ccBody) > 200 {
				message = ccBody[:200]
			} else {
				message = ccBody
			}
			if message == "" {
				message = fmt.Sprintf("CC API error (%d)", ccStatus)
			}
		}
	}

	return finishMapped(mapped, ccStatus, message, code)
}

var statusPrefixRe = regexp.MustCompile(`^<(\d{3})>`)

// MapCcEventError translates an in-stream {"type":"error"} event.
// Precedence (aligned with the CLI's readStreamErrorEvent):
// "<NNN>" prefix in message → error.statusCode → 502.
func MapCcEventError(e *CCEvent) *MappedError {
	message := "Unknown CC error"
	if e.Error != nil && e.Error.Message != "" {
		message = e.Error.Message
	} else if e.Message != "" {
		message = e.Message
	}
	code := ""
	if e.Error != nil {
		code = e.Error.Code
	}
	if code == "" {
		code = e.Code
	}

	reportedStatus := 0
	if m := statusPrefixRe.FindStringSubmatch(message); m != nil {
		fmt.Sscanf(m[1], "%d", &reportedStatus)
	} else if e.Error != nil && e.Error.StatusCode != nil {
		reportedStatus = *e.Error.StatusCode
	}

	ccStatus := reportedStatus
	if ccStatus == 0 {
		ccStatus = 502
	}
	return finishMapped(mapStatus(ccStatus), ccStatus, message, code)
}

func finishMapped(mapped statusTarget, ccStatus int, message, code string) *MappedError {
	if mapped.Status == 429 {
		return &MappedError{
			Status:         429,
			Code:           code,
			ReportedStatus: ccStatus,
			Body: ErrorBody{
				Error:      ErrorDetail{Message: message, Type: "rate_limit_error", Code: code},
				RetryAfter: 30,
			},
		}
	}
	return &MappedError{
		Status:         mapped.Status,
		Code:           code,
		ReportedStatus: ccStatus,
		Body:           ErrorBody{Error: ErrorDetail{Message: message, Type: mapped.Type, Code: code}},
	}
}

// IncompleteUpstreamError builds the retryable 502 for a stream that never
// finished normally (port of incompleteUpstreamError).
func IncompleteUpstreamError(detail string) *MappedError {
	return &MappedError{
		Status: 502,
		Body: ErrorBody{
			Error: ErrorDetail{
				Message: fmt.Sprintf("Upstream stream ended without a completion finish (%s) — response was truncated", detail),
				Type:    "upstream_error",
			},
			RetryAfter: 10,
		},
	}
}
