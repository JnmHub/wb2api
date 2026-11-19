package codearts

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// ChatError is a CodeArts business fault carried in a successful HTTP reply.
// Body is used internally for routing and diagnostics. Error exposes only the
// vendor code so a fault cannot leak arbitrary upstream details to clients.
type ChatError struct {
	Status int
	Code   string
	Body   []byte
}

func (e *ChatError) Error() string {
	if e.Code == "" {
		return "CodeArts chat failed"
	}
	return "CodeArts chat failed (" + e.Code + ")"
}

// ParseChatError identifies both the CodeArts business envelope and the
// OpenAI-compatible error object used by some upstream routes.
func ParseChatError(data []byte) *ChatError {
	var frame struct {
		Code  json.RawMessage `json:"error_code"`
		Msg   string          `json:"error_msg"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &frame) != nil {
		return nil
	}
	code := chatErrorCode(frame.Code)
	switch strings.ToLower(code) {
	case "0", "00", "0000", "success", "ok":
		code = ""
	}
	detail := frame.Msg
	if raw := string(bytes.TrimSpace(frame.Error)); raw != "" && raw != "null" && raw != "false" && raw != `""` {
		var nested struct {
			Code    json.RawMessage `json:"code"`
			Type    string          `json:"type"`
			Message string          `json:"message"`
		}
		if json.Unmarshal([]byte(raw), &nested) == nil {
			if code == "" {
				code = chatErrorCode(nested.Code)
				if code == "" {
					code = nested.Type
				}
			}
			if detail == "" {
				detail = nested.Message
			}
		}
	} else if code == "" {
		return nil
	}

	return &ChatError{Status: chatErrorStatus(code, detail, data), Code: safeChatErrorCode(code), Body: bytes.Clone(data)}
}

func safeChatErrorCode(code string) string {
	if len(code) > 96 {
		return ""
	}
	for _, c := range code {
		if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '.' && c != '_' && c != '-' {
			return ""
		}
	}
	return code
}

func chatErrorCode(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	if _, err := strconv.Atoi(string(raw)); err == nil {
		return string(raw)
	}
	return ""
}

func chatErrorStatus(code, detail string, body []byte) int {
	code = strings.ToLower(code)
	detail = strings.ToLower(detail)
	switch {
	case code == "inferhub.4291.200", code == "insufficient_quota",
		strings.Contains(detail, "insufficient quota"), strings.Contains(detail, "额度已用尽"):
		return http.StatusPaymentRequired
	case IsModelAccessError(body):
		return http.StatusForbidden
	case IsFatalRequestError(body), code == "invalid_request_error":
		return http.StatusBadRequest
	case code == "invalid_api_key", code == "authentication_error":
		return http.StatusUnauthorized
	case code == "rate_limit_exceeded", strings.Contains(code, ".429"), strings.Contains(detail, "rate limit"):
		return http.StatusTooManyRequests
	}
	last := code[strings.LastIndexByte(code, '.')+1:]
	if status, err := strconv.Atoi(last); err == nil && status >= 400 && status <= 599 {
		return status
	}
	return http.StatusBadGateway
}
