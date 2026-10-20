package openai

import "strings"

// gatewayHint is a gateway-side note that sits beside the raw upstream message.
// It never replaces the upstream wording (clients keep parsing the same
// envelope); it only explains what the gateway could classify, so a user
// staring at "400 Bad Request" learns whether the fix is a smaller context, a
// different model, a re-login or simply waiting. "" = nothing to add.
func gatewayHint(status int, message string) string {
	lower := strings.ToLower(message)
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(lower, s) {
				return true
			}
		}
		return false
	}
	switch {
	case status == 413 && has("payload too large"):
		return "single request body exceeds the gateway cap; raise -max-body-mb (or WB_MAX_BODY_MB) and restart"
	case status == 503 && has("concurrent chat limit"):
		return "gateway is busy at its concurrency limit; retry shortly"
	case has("11133", "model_param_invalid", "invalid request parameters"):
		return "request parameters were rejected by the model provider; check message format and model capabilities"
	case has("11135", "invalid_image_data", "replace the image"):
		return "image data rejected by upstream; use a real/valid image, may need a new conversation"
	case has("11140", "account has been restricted", "unusual activity") || strings.Contains(message, "账号存在异常行为"):
		return "上游已对该账号做账号级风控限制（403 / code 11140）；网关已让该账号冷却并停止换号重试。" +
			"请勿换号或高频重试（同设备账号会连坐），按 WorkBuddy 官方渠道申诉，通常 1-3 个工作日自动解除。"
	case has("no usable account", "no healthy account"):
		return "no healthy account available in pool; check the dashboard or retry later"
	case strings.Contains(message, "本机网络不可用") || has("local network unavailable"):
		return "this machine has no working network (DNS/route failed); accounts were not penalised - check the host's network or proxy"
	case has("context length", "context_length", "prompt too long", "too many tokens", "maximum context"):
		return "request context exceeds the model's limit; reduce history/message size"
	case status == 429 || has("rate limit", "frequency limit"):
		return "rate limited by upstream; retry after reset"
	case status == 402 || (has("insufficient") && has("credit")):
		return "account credits exhausted at upstream; waiting for daily check-in to restore"
	case has("session") && (has("not found") || has("expired")):
		return "account session expired at upstream; the account is disabled until re-login"
	case has("content") && (has("reject") || has("policy")):
		return "request content was rejected by content policy; adjust the prompt and retry"
	case has("no such model", "model not found", "unsupported model"):
		return "upstream has no such model on this backend; switch model or retry on another account"
	case status == 403 && has("waf"):
		return "upstream WAF blocked the gateway; retry after the block window"
	}
	return ""
}
