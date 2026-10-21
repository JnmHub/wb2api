package openai

import (
	"strconv"
	"strings"

	"wb2api/internal/jsonx"
)

// Upstream bodies that say the model/service is unavailable, not that this
// credential is bad. They arrive as HTTP 5xx for some upstream deployments; the
// numeric code is the only reliable discriminator. Charging them to the account
// lets three requests against a dead model name trip the whole pool's breaker.
var modelScopedErrorCodes = map[int]bool{
	11102: true, // service info not found
	11129: true, // invalid function parameters
	11134: true, // model provider temporarily unavailable
	11150: true, // reasoning effort unsupported
	11158: true, // model not available to this account
}

func modelScopedErrorCode(body []byte) int {
	obj, err := jsonx.UnmarshalObj(body)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(jsonx.PyStr(obj.Get("code"))))
	if err != nil {
		return 0
	}
	return n
}
