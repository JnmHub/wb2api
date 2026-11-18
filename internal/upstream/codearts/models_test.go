package codearts

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// discoveryServer emulates the three catalogue sources.
func discoveryServer(t *testing.T, opts discoveryOpts) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == AgentListPath:
			if opts.agentListErr {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `{"error_code":"denied"}`)
				return
			}
			fmt.Fprintf(w, `{"agents":[
				{"agent_id":"a-other","agent_name":"Other","show_in_ide":true},
				{"agent_id":"a-code","agent_name":"CodeAgent","show_in_ide":true,"alias":{"alias_zh_cn":"智能体"}}]}`)
		case r.URL.Path == AgentDetailPath:
			if r.URL.Query().Get("agent_id") != "a-code" {
				t.Errorf("detail agent_id = %q", r.URL.Query().Get("agent_id"))
			}
			if r.Header.Get("Agent-Type") != "AgentCenter" {
				t.Errorf("detail Agent-Type = %q", r.Header.Get("Agent-Type"))
			}
			fmt.Fprint(w, `{"gpts":{"models":[
				{"model_name":"GLM-5.2","model_id":"glm-5.2","model_parameters":{"context_window":131072,"max_tokens":8192,"model_desc":"主力"}}]}}`)
		case r.URL.Path == BenefitGatePath:
			fmt.Fprint(w, `{"enabled":true}`)
		case r.URL.Path == BuiltinPath:
			if r.Header.Get("Agent-Type") != "PromptCenter" {
				t.Errorf("builtin Agent-Type = %q", r.Header.Get("Agent-Type"))
			}
			fmt.Fprint(w, `{"builtinModels":[
				{"model_id":"Qwen3-VL-235B","context_window":262144},
				{"model_id":"GLM-5.2","model_name":"GLM-5.2"}]}`)
		case r.URL.Path == BenefitConfigPath:
			if opts.benefitErrCode != "" {
				fmt.Fprintf(w, `{"error_code":%q,"error_msg":"no plan"}`, opts.benefitErrCode)
				return
			}
			fmt.Fprint(w, `{"error_code":"0000","result":{"models":[
				{"model_id":"deepseek-v4.1-flash-1017","context_window":131072}]}}`)
		case r.URL.Path == BenefitClaimPath:
			if opts.claimErr {
				fmt.Fprint(w, `{"error_code":"4004","error_msg":"claim rejected"}`)
				return
			}
			if r.Method != http.MethodPost {
				t.Errorf("claim method = %s", r.Method)
			}
			fmt.Fprint(w, `{"error_code":"0000"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

type discoveryOpts struct {
	agentListErr   bool
	claimErr       bool
	benefitErrCode string
}

func TestDiscoverModelsMergesThreeSources(t *testing.T) {
	srv := discoveryServer(t, discoveryOpts{})
	dis := DiscoverModelsWith(context.Background(), srv.Client(), srv.URL, srv.URL, "u-1", CreditCredential(), true)
	if len(dis.Errors) != 0 {
		t.Fatalf("errors = %v", dis.Errors)
	}
	if !dis.Claimed {
		t.Fatal("benefit claim was not attempted")
	}
	byID := map[string]ModelInfo{}
	for _, m := range dis.Models {
		byID[strings.ToLower(m.ID)] = m
	}
	if _, ok := byID["glm-5.2"]; !ok {
		t.Fatalf("agent/builtin model missing: %+v", dis.Models)
	}
	if _, ok := byID["qwen3-vl-235b"]; !ok {
		t.Fatalf("builtin model missing: %+v", dis.Models)
	}
	b, ok := byID["deepseek-v4.1-flash-1017"]
	if !ok {
		t.Fatalf("benefit model missing: %+v", dis.Models)
	}
	if !b.Benefit {
		t.Fatal("benefit flag lost: the chat request would miss maas_type")
	}
	// The agent source spells the model "GLM-5.2"; the merge prefers the exact
	// spelling over the lowercase duplicate.
	if got := byID["glm-5.2"]; got.ID != "GLM-5.2" {
		t.Fatalf("merged id = %q, want the capitalised spelling", got.ID)
	}
	if byID["glm-5.2"].ContextWindow == 0 {
		t.Fatal("merged entry lost the context window")
	}
}

// A failing source must not block the others: the catalogue is a union.
func TestDiscoverModelsSurvivesPartialFailure(t *testing.T) {
	srv := discoveryServer(t, discoveryOpts{agentListErr: true, benefitErrCode: "4004"})
	dis := DiscoverModelsWith(context.Background(), srv.Client(), srv.URL, srv.URL, "u-1", CreditCredential(), false)
	if len(dis.Models) == 0 {
		t.Fatal("builtin models should still be returned")
	}
	if _, ok := dis.Errors["agent"]; !ok {
		t.Fatalf("agent failure not reported: %v", dis.Errors)
	}
	if _, ok := dis.Errors["benefit"]; !ok {
		t.Fatalf("benefit failure not reported: %v", dis.Errors)
	}
	if dis.Claimed {
		t.Fatal("claim must not run when claimBenefit is false")
	}
}

func TestClaimBenefitRejectsBusinessError(t *testing.T) {
	srv := discoveryServer(t, discoveryOpts{claimErr: true})
	if err := ClaimBenefitAt(context.Background(), srv.Client(), srv.URL, CreditCredential()); err == nil {
		t.Fatal("a non-zero error_code must be an error")
	}
}

func TestMergeModelsKeepsBenefitFlagAndRicherEntry(t *testing.T) {
	got := MergeModels(
		[]ModelInfo{{ID: "GLM-5.2", ContextWindow: 1000}},
		[]ModelInfo{{ID: "glm-5.2", Benefit: true}},
		[]ModelInfo{{ID: "GLM-5.2", MaxTokens: 42}},
	)
	if len(got) != 1 {
		t.Fatalf("merge produced %d entries: %+v", len(got), got)
	}
	m := got[0]
	if !m.Benefit {
		t.Fatal("benefit flag must be OR-ed")
	}
	if m.ContextWindow != 1000 || m.MaxTokens != 42 {
		t.Fatalf("params lost: %+v", m)
	}
	if m.ID != "GLM-5.2" {
		t.Fatalf("exact spelling lost: %q", m.ID)
	}
}

func TestMergeModelsSortsCaseInsensitively(t *testing.T) {
	got := MergeModels([]ModelInfo{{ID: "b"}, {ID: "A"}, {ID: "a"}, {ID: "B"}})
	// One entry per case-insensitive id, sorted, keeping the exact spelling.
	if len(got) != 2 || got[0].ID != "A" || got[1].ID != "B" {
		t.Fatalf("got %+v", got)
	}
}

// CreditCredential is the fixture used by the discovery and login tests.
func CreditCredential() Credential {
	return Credential{AccessKeyID: "AK", SecretAccessKey: "SK", SecurityToken: "STS"}
}

// discoveryServer must decode the benefit config the same way the real gateway
// does; this guards the envelope shape.
func TestBenefitConfigEnvelope(t *testing.T) {
	srv := discoveryServer(t, discoveryOpts{})
	raw, err := SignedGet(context.Background(), srv.Client(), srv.URL+BenefitConfigPath, CreditCredential(), "")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		ErrorCode string `json:"error_code"`
		Result    struct {
			Models []struct {
				ModelID string `json:"model_id"`
			} `json:"models"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.ErrorCode != "0000" || len(out.Result.Models) == 0 {
		t.Fatalf("unexpected envelope: %s", raw)
	}
}

// An account without a free-plan group answers code=4000; that is a capability
// fact, not a discovery failure.
func TestDiscoverTreatsNoBenefitGroupAsEmpty(t *testing.T) {
	srv := discoveryServer(t, discoveryOpts{})
	// override the benefit endpoints with a 4000 answer
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case AgentListPath:
			fmt.Fprint(w, `{"agents":[{"agent_id":"a-1","agent_name":"CodeAgent","show_in_ide":true,"alias":{"alias_zh_cn":"智能体"}}]}`)
		case AgentDetailPath, BuiltinPath:
			fmt.Fprint(w, `{"builtinModels":[],"gpts":{"models":[]}}`)
		case BenefitGatePath:
			fmt.Fprint(w, `{"enabled":true}`)
		case BenefitClaimPath, BenefitConfigPath:
			fmt.Fprint(w, `{"error_code":"4000","error_msg":"invalid channel, group not found"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer bad.Close()
	defer srv.Close()

	dis := DiscoverModelsWith(context.Background(), bad.Client(), bad.URL, bad.URL, "u-1", CreditCredential(), true)
	if _, ok := dis.Errors["benefit"]; ok {
		t.Fatalf("no-entitlement must not be reported as an error: %v", dis.Errors)
	}
	if _, ok := dis.Errors["benefit_claim"]; ok {
		t.Fatalf("no-entitlement claim must not be reported: %v", dis.Errors)
	}
}

// Broken UTF-8 ids in the upstream catalogue must not reach /v1/models.
func TestFirstNonEmptySkipsInvalidUTF8(t *testing.T) {
	if got := firstNonEmpty("bad\x80id", "GLM-5.2"); got != "GLM-5.2" {
		t.Fatalf("got %q, want the valid candidate", got)
	}
	if got := firstNonEmpty("\x80\x81"); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

// A disabled benefit gateway means "no free-plan catalogue here": the switch is
// honoured and nothing is reported as a failure.
func TestDiscoverSkipsBenefitWhenGateDisabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case BenefitGatePath:
			fmt.Fprint(w, `{"enabled":false}`)
		case AgentListPath:
			fmt.Fprint(w, `{"agents":[{"agent_id":"a-1","agent_name":"CodeAgent","show_in_ide":true,"alias":{"alias_zh_cn":"智能体"}}]}`)
		case AgentDetailPath:
			fmt.Fprint(w, `{"gpts":{"models":[{"model_name":"GLM-5.2"}]}}`)
		case BuiltinPath:
			fmt.Fprint(w, `{"builtinModels":[]}`)
		default:
			t.Errorf("the benefit endpoints must not be called when the gate is off: %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()

	dis := DiscoverModelsWith(context.Background(), srv.Client(), srv.URL, srv.URL, "u-1", CreditCredential(), true)
	if len(dis.Errors) != 0 {
		t.Fatalf("errors = %v", dis.Errors)
	}
	if dis.Claimed {
		t.Fatal("a disabled gate must not trigger a claim")
	}
	if len(dis.Models) != 1 || dis.Models[0].ID != "GLM-5.2" {
		t.Fatalf("models = %+v", dis.Models)
	}
}
