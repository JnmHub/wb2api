package openai

import (
	"strings"
	"testing"
)

// A Huawei error frame arrives inside an HTTP 200 stream as a bare
// {"error_code","error_msg"} object. Treating it as content would report a
// false success and block failover.
func TestUpstreamSSEErrorRecognisesCodeArtsEnvelope(t *testing.T) {
	err, is := upstreamSSEError([]byte(`{"error_code":"InferHub.4291.200","error_msg":"insufficient quota"}`))
	if !is || err == nil {
		t.Fatal("the error frame was not recognised")
	}
	if !strings.Contains(err.Error(), "insufficient quota") || !strings.Contains(err.Error(), "InferHub.4291.200") {
		t.Fatalf("err = %v", err)
	}

	// Business codes that mean "fine" must not be treated as faults.
	for _, body := range []string{
		`{"error_code":"0"}`,
		`{"error_code":"0000"}`,
		`{"error_code":"success"}`,
		`{"error_code":""}`,
		`{"choices":[{"delta":{"content":"hi"}}]}`,
	} {
		if _, is := upstreamSSEError([]byte(body)); is {
			t.Fatalf("%s was classified as an error", body)
		}
	}

	// A bare code without a message still carries the code.
	err, is = upstreamSSEError([]byte(`{"error_code":"TM.00001041"}`))
	if !is || !strings.Contains(err.Error(), "TM.00001041") {
		t.Fatalf("code-only frame = %v", err)
	}

	// The OpenAI-shaped envelope keeps working.
	if _, is := upstreamSSEError([]byte(`{"error":{"message":"boom","type":"server_error"}}`)); !is {
		t.Fatal("openai envelope regressed")
	}
}
