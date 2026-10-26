package openai

import (
	"strconv"
	"strings"
	"time"

	"wb2api/internal/jsonx"
)

// entitlementBlock is how long an account stops being picked for a model the
// upstream refused with 11158 ("model not available to this account"). The
// upstream answers the same way until the account's tier changes, so a short
// window would only buy repeated 5xx for the same model.
const entitlementBlock = 24 * time.Hour

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
