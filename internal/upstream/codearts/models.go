package codearts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"wb2api/internal/upstream/identity"
)

// Model-discovery endpoints. The agent-centre and builtin sources live on
// snap-access; the benefit (free plan) gateway is a separate host.
const (
	AgentListPath   = "/v1/agent-center/agents/useragents"
	AgentDetailPath = "/v1/agent-center/agents/detail"
	BuiltinPath     = "/v1/model/builtin"
	// BenefitGatePath is the on/off switch of the free-plan gateway. It lives on
	// snap-access; the catalogue itself is on another host.
	BenefitGatePath = "/v1/benefit-gateway-config"

	// BenefitBase hosts the limited-time free-plan models.
	BenefitBase       = "https://opengw.developer.huaweicloud.com"
	BenefitConfigPath = "/api/v1/gateway/config"
	BenefitClaimPath  = "/api/v1/benefit/claim"

	// CurrentUserPath identifies the account behind an STS token.
	CurrentUserPath = "/snap-manager/v1/current/user"
)

// ModelInfo is one entry of an account's catalogue.
type ModelInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	ContextWindow int64  `json:"context_window,omitempty"`
	MaxTokens     int64  `json:"max_tokens,omitempty"`
	Desc          string `json:"desc,omitempty"`
	// Benefit marks a limited-time free-plan model: a chat request for it must
	// carry the maas_type: benefit header.
	Benefit bool `json:"benefit,omitempty"`
}

// Discovery is the merged result of the three sources plus per-source failures.
// A failing source never blocks the others: the catalogue is a union, and the
// upstream really does 404 on models the account never registered.
type Discovery struct {
	Models  []ModelInfo
	Errors  map[string]string
	Claimed bool
}

// SignedGet performs one SDK-HMAC-SHA256 signed GET and returns the body.
func SignedGet(ctx context.Context, hc *http.Client, rawURL string, cred Credential, agentType string) ([]byte, error) {
	body := []byte{}
	h := identity.Headers{
		identity.P("Content-Type", "application/json"),
		identity.P("X-Language", "zh-cn"),
	}
	if agentType != "" {
		h = append(h, identity.P("Agent-Type", agentType))
	}
	h = Signer{}.SignHeaders(h, http.MethodGet, rawURL, body, cred)
	return doSigned(ctx, hc, rawURL, http.MethodGet, h, body)
}

func doSigned(ctx context.Context, hc *http.Client, rawURL, method string, h identity.Headers, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for _, p := range h {
		if strings.EqualFold(p.Name, "Host") {
			continue // net/http writes Host from the URL
		}
		req.Header.Set(p.Name, p.Value)
	}
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("codearts %s HTTP %d: %s", req.URL.Path, resp.StatusCode, truncate(string(raw), 300))
	}
	return raw, nil
}

// DiscoverModels unions the three catalogue sources for one account.
// claimBenefit makes the free-plan models usable: without the claim the
// upstream answers InferHub.4004.200 on the first chat request.
func DiscoverModels(ctx context.Context, hc *http.Client, base, accountID string, cred Credential, claimBenefit bool) Discovery {
	return DiscoverModelsWith(ctx, hc, base, BenefitBase, accountID, cred, claimBenefit)
}

// DiscoverModelsWith is DiscoverModels with an explicit free-plan gateway root
// (tests point it at a fake server).
func DiscoverModelsWith(ctx context.Context, hc *http.Client, base, benefitBase, accountID string, cred Credential, claimBenefit bool) Discovery {
	out := Discovery{Errors: map[string]string{}}
	var sets [][]ModelInfo

	if infos, err := fetchBuiltinModels(ctx, hc, base, cred); err != nil {
		out.Errors["builtin"] = err.Error()
	} else if len(infos) > 0 {
		// First wins on a case-insensitive collision: model_id is what the chat
		// endpoint accepts, while the agent source only has display names.
		sets = append(sets, infos)
	}
	if infos, err := fetchAgentModels(ctx, hc, base, cred); err != nil {
		out.Errors["agent"] = err.Error()
	} else if len(infos) > 0 {
		sets = append(sets, infos)
	}
	// The benefit gateway has its own on/off switch. A deployment that does not
	// have it answers "disabled" rather than an error, so a disabled switch is
	// not reported as a discovery failure.
	benefitOn := true
	if gate := fetchBenefitGate(ctx, hc, base, cred); gate != nil {
		benefitOn = *gate
	}
	if benefitOn {
		if claimBenefit {
			if err := ClaimBenefitAt(ctx, hc, benefitBase, cred); err != nil {
				out.Errors["benefit_claim"] = err.Error()
			} else {
				out.Claimed = true
			}
		}
		if infos, err := fetchBenefitModels(ctx, hc, benefitBase, cred); err != nil {
			out.Errors["benefit"] = err.Error()
		} else if len(infos) > 0 {
			sets = append(sets, infos)
		}
	}

	out.Models = MergeModels(sets...)
	if len(out.Models) == 0 {
		if len(out.Errors) == 0 {
			out.Errors["all"] = "所有来源都没有返回模型"
		}
		return out
	}
	return out
}

// fetchAgentModels reads the models registered to the account's default agent.
func fetchAgentModels(ctx context.Context, hc *http.Client, base string, cred Credential) ([]ModelInfo, error) {
	agentID, err := defaultAgentID(ctx, hc, base, cred)
	if err != nil {
		return nil, err
	}
	if agentID == "" {
		return nil, fmt.Errorf("账号没有默认 Agent")
	}
	raw, err := SignedGet(ctx, hc, strings.TrimRight(base, "/")+AgentDetailPath+"?agent_id="+url.QueryEscape(agentID), cred, "AgentCenter")
	if err != nil {
		return nil, err
	}
	var detail struct {
		Gpts struct {
			Models []struct {
				ModelAlias string `json:"model_alias"`
				ModelName  string `json:"model_name"`
				ModelID    string `json:"model_id"`
				Params     struct {
					ContextWindow int64  `json:"context_window"`
					MaxTokens     int64  `json:"max_tokens"`
					ModelID       string `json:"model_id"`
					ModelDesc     string `json:"model_desc"`
				} `json:"model_parameters"`
			} `json:"models"`
		} `json:"gpts"`
	}
	if err := json.Unmarshal(raw, &detail); err != nil {
		return nil, fmt.Errorf("parse agent models: %w", err)
	}
	out := make([]ModelInfo, 0, len(detail.Gpts.Models))
	for _, m := range detail.Gpts.Models {
		id := firstNonEmpty(m.ModelName, m.ModelID, m.ModelAlias, m.Params.ModelID)
		if id == "" {
			continue
		}
		out = append(out, ModelInfo{ID: id, Name: id, ContextWindow: m.Params.ContextWindow, MaxTokens: m.Params.MaxTokens, Desc: m.Params.ModelDesc})
	}
	return out, nil
}

// defaultAgentID picks the same agent the official client shows: CodeAgent with
// the "智能体" alias, then the primary agent, then the first one.
func defaultAgentID(ctx context.Context, hc *http.Client, base string, cred Credential) (string, error) {
	raw, err := SignedGet(ctx, hc, strings.TrimRight(base, "/")+AgentListPath+"?offset=0&limit=100&is_primary_agent=true", cred, "AgentCenter")
	if err != nil {
		return "", err
	}
	var out struct {
		Agents []struct {
			AgentID   string `json:"agent_id"`
			AgentName string `json:"agent_name"`
			Primary   bool   `json:"is_primary_agent"`
			ShowInIDE bool   `json:"show_in_ide"`
			Alias     struct {
				ZhCN string `json:"alias_zh_cn"`
			} `json:"alias"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("parse agents: %w", err)
	}
	for _, a := range out.Agents {
		if a.AgentName == "CodeAgent" && a.Alias.ZhCN == "智能体" && a.ShowInIDE {
			return a.AgentID, nil
		}
	}
	for _, a := range out.Agents {
		if a.Primary {
			return a.AgentID, nil
		}
	}
	if len(out.Agents) > 0 {
		return out.Agents[0].AgentID, nil
	}
	return "", nil
}

// fetchBuiltinModels lists the built-in models (Agent-Type: PromptCenter).
func fetchBuiltinModels(ctx context.Context, hc *http.Client, base string, cred Credential) ([]ModelInfo, error) {
	raw, err := SignedGet(ctx, hc, strings.TrimRight(base, "/")+BuiltinPath, cred, "PromptCenter")
	if err != nil {
		return nil, err
	}
	var out struct {
		BuiltinModels []struct {
			ModelID       string `json:"model_id"`
			ModelName     string `json:"model_name"`
			ModelDesc     string `json:"model_desc"`
			ContextWindow int64  `json:"context_window"`
			MaxTokens     int64  `json:"max_tokens"`
		} `json:"builtinModels"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse builtin models: %w", err)
	}
	infos := make([]ModelInfo, 0, len(out.BuiltinModels))
	for _, m := range out.BuiltinModels {
		id := firstNonEmpty(m.ModelID, m.ModelName)
		if id == "" {
			continue
		}
		// Keep the display name: it is the second key the merge uses, and it is
		// what WireName translates back to the callable model_id.
		infos = append(infos, ModelInfo{ID: id, Name: firstNonEmpty(m.ModelName, id), ContextWindow: m.ContextWindow, MaxTokens: m.MaxTokens, Desc: m.ModelDesc})
	}
	return infos, nil
}

// ClaimBenefit claims the limited-time free-plan models. The upstream treats it
// as idempotent (the official client calls it whenever the model menu opens),
// so calling it on every discovery pass is safe.
func ClaimBenefit(ctx context.Context, hc *http.Client, cred Credential) error {
	return ClaimBenefitAt(ctx, hc, BenefitBase, cred)
}

// ClaimBenefitAt claims the free-plan models against an explicit gateway root.
func ClaimBenefitAt(ctx context.Context, hc *http.Client, benefitBase string, cred Credential) error {
	body := []byte("{}")
	target := strings.TrimRight(benefitBase, "/") + BenefitClaimPath
	h := identity.Headers{
		identity.P("Content-Type", "application/json"),
		identity.P("X-Language", "zh-cn"),
	}
	h = Signer{}.SignHeaders(h, http.MethodPost, target, body, cred)
	raw, err := doSigned(ctx, hc, target, http.MethodPost, h, body)
	if err != nil {
		return err
	}
	var out struct {
		ErrorCode string `json:"error_code"`
		ErrorMsg  string `json:"error_msg"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("parse benefit claim: %w", err)
	}
	if out.ErrorCode != "" && out.ErrorCode != "0000" {
		if isNoBenefitGroup(out.ErrorCode, out.ErrorMsg) {
			return nil // the account has no free-plan group: nothing to claim
		}
		return fmt.Errorf("benefit claim failed: code=%s msg=%s", out.ErrorCode, out.ErrorMsg)
	}
	return nil
}

// fetchBenefitModels lists the free-plan models.
func fetchBenefitModels(ctx context.Context, hc *http.Client, benefitBase string, cred Credential) ([]ModelInfo, error) {
	raw, err := SignedGet(ctx, hc, strings.TrimRight(benefitBase, "/")+BenefitConfigPath, cred, "")
	if err != nil {
		return nil, err
	}
	var out struct {
		ErrorCode string `json:"error_code"`
		ErrorMsg  string `json:"error_msg"`
		Result    struct {
			BaseURL string `json:"base_url"`
			Models  []struct {
				ModelID       string `json:"model_id"`
				ModelName     string `json:"model_name"`
				ModelDesc     string `json:"model_desc"`
				ContextWindow int64  `json:"context_window"`
				MaxTokens     int64  `json:"max_tokens"`
			} `json:"models"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse benefit config: %w", err)
	}
	if out.ErrorCode != "" && out.ErrorCode != "0000" {
		if isNoBenefitGroup(out.ErrorCode, out.ErrorMsg) {
			return nil, nil // no free-plan entitlement
		}
		return nil, fmt.Errorf("benefit gateway code=%s msg=%s", out.ErrorCode, out.ErrorMsg)
	}
	infos := make([]ModelInfo, 0, len(out.Result.Models))
	for _, m := range out.Result.Models {
		id := firstNonEmpty(m.ModelID, m.ModelName)
		if id == "" {
			continue
		}
		infos = append(infos, ModelInfo{ID: id, Name: id, ContextWindow: m.ContextWindow, MaxTokens: m.MaxTokens, Desc: m.ModelDesc, Benefit: true})
	}
	return infos, nil
}

// MergeModels unions model sets. The benefit flag is OR-ed (a lost flag means a
// lost maas_type header and a 404 at chat time); otherwise the richer entry wins.
func MergeModels(sets ...[]ModelInfo) []ModelInfo {
	merged := map[string]ModelInfo{}
	// The same model can arrive under two spellings (builtin's model_id and the
	// agent source's display name), so the display name is a second key: without
	// it "glm-5.2-sft-harmony" and "GLM-5.2-ArkTS-SPARK" would both be listed.
	byName := map[string]string{}
	for _, set := range sets {
		for _, mi := range set {
			if mi.ID == "" {
				continue
			}
			key := strings.ToLower(mi.ID)
			if alias, ok := byName[strings.ToLower(mi.ID)]; ok {
				key = alias
			} else if mi.Name != "" {
				if alias, ok := byName[strings.ToLower(mi.Name)]; ok {
					key = alias
				}
			}
			cur, ok := merged[key]
			if !ok {
				merged[key] = mi
				byName[strings.ToLower(mi.ID)] = key
				if mi.Name != "" {
					byName[strings.ToLower(mi.Name)] = key
				}
				continue
			}
			merged[key] = richerModel(cur, mi)
		}
	}
	out := make([]ModelInfo, 0, len(merged))
	for _, mi := range merged {
		out = append(out, mi)
	}
	sort.Slice(out, func(i, j int) bool {
		li, lj := strings.ToLower(out[i].ID), strings.ToLower(out[j].ID)
		if li != lj {
			return li < lj
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func richerModel(cur, next ModelInfo) ModelInfo {
	benefit := cur.Benefit || next.Benefit
	keep, drop := cur, next
	if betterModel(next, cur) {
		keep, drop = next, cur
	}
	keep.Benefit = benefit
	keep.ID = cur.ID // the first source owns the wire spelling
	if keep.Name == "" {
		keep.Name = drop.Name
	}
	if keep.Desc == "" {
		keep.Desc = drop.Desc
	}
	if keep.ContextWindow == 0 {
		keep.ContextWindow = drop.ContextWindow
	}
	if keep.MaxTokens == 0 {
		keep.MaxTokens = drop.MaxTokens
	}
	return keep
}

// betterModel prefers an entry with parameters, then the exact spelling.
func betterModel(a, b ModelInfo) bool {
	if (a.ContextWindow > 0) != (b.ContextWindow > 0) {
		return a.ContextWindow > 0
	}
	return a.ID != strings.ToLower(a.ID) && b.ID == strings.ToLower(b.ID)
}

// isNoBenefitGroup reports the "account is not in a free-plan group" answer.
// It is a capability fact, not an error worth showing on the account.
func isNoBenefitGroup(code, msg string) bool {
	return code == "4000" || strings.Contains(strings.ToLower(msg), "group not found") ||
		strings.Contains(msg, "invalid channel")
}

// firstNonEmpty returns the first non-blank value that is also valid UTF-8: the
// upstream catalogue carries at least one entry whose id bytes are broken, and
// advertising it would only break clients.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		s := strings.TrimSpace(v)
		if s != "" && utf8.ValidString(s) {
			return s
		}
	}
	return ""
}

// fetchBenefitGate reads the free-plan switch. A nil result means the endpoint
// does not exist (older deployment): callers then behave as before.
func fetchBenefitGate(ctx context.Context, hc *http.Client, base string, cred Credential) *bool {
	raw, err := SignedGet(ctx, hc, strings.TrimRight(base, "/")+BenefitGatePath, cred, "")
	if err != nil {
		return nil
	}
	var out struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Enabled == nil {
		return nil
	}
	return out.Enabled
}
