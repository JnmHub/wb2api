package api

import (
	"context"
	"net/http"
	"time"

	"wb2api/internal/pricing"
)

func (u *Usage) pricingGet(w http.ResponseWriter, r *http.Request) {
	if u.Pricing == nil {
		writeErr(w, http.StatusServiceUnavailable, "价格估算不可用")
		return
	}
	writeJSON(w, http.StatusOK, u.Pricing.Status())
}

func (u *Usage) pricingRefresh(w http.ResponseWriter, r *http.Request) {
	if u.Pricing == nil {
		writeErr(w, http.StatusServiceUnavailable, "价格估算不可用")
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 90*time.Second)
	defer cancel()
	n, err := u.Pricing.Refresh(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "刷新失败: "+err.Error())
		return
	}
	st := u.Pricing.Status()
	st["fetched"] = n
	writeJSON(w, http.StatusOK, st)
}

type pricingPolicyReq struct {
	Model      string   `json:"model"`
	Mode       string   `json:"mode"`
	ORID       string   `json:"or_id"`
	Input      *float64 `json:"input"`
	Output     *float64 `json:"output"`
	CacheRead  *float64 `json:"cache_read"`
	CacheWrite *float64 `json:"cache_write"`
	Note       string   `json:"note"`
}

func (u *Usage) pricingPolicy(w http.ResponseWriter, r *http.Request) {
	if u.Pricing == nil {
		writeErr(w, http.StatusServiceUnavailable, "价格估算不可用")
		return
	}
	var in pricingPolicyReq
	if !decode(w, r, &in) {
		return
	}
	err := u.Pricing.SetPolicy(in.Model, pricing.Policy{Mode: in.Mode, ORID: in.ORID,
		Input: in.Input, Output: in.Output, CacheRead: in.CacheRead, CacheWrite: in.CacheWrite, Note: in.Note})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, u.Pricing.Status())
}

func (u *Usage) pricingCost(w http.ResponseWriter, r *http.Request) {
	if u.Pricing == nil {
		writeErr(w, http.StatusServiceUnavailable, "价格估算不可用")
		return
	}
	f, err := u.FilterFromQuery(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	groups, err := u.DB.Group("model", f)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	type row struct {
		Model      string  `json:"model"`
		Requests   int64   `json:"requests"`
		Prompt     int64   `json:"prompt_tokens"`
		Completion int64   `json:"completion_tokens"`
		Cached     int64   `json:"cached_tokens"`
		CostCNY    float64 `json:"cost_cny"`
		CostUSD    float64 `json:"cost_usd"`
		Priced     bool    `json:"priced"`
		Source     string  `json:"source"`
	}
	out := make([]row, 0, len(groups))
	var totalCNY, totalUSD float64
	var pricedModels, unpriced int
	for _, g := range groups {
		rw := row{Model: g.Key, Requests: g.Requests, Prompt: g.Prompt, Completion: g.Completion, Cached: g.Cached}
		if cny, usd, ok := u.Pricing.Cost(g.Key, g.Prompt, g.Completion, g.Cached, 0); ok {
			rw.CostCNY, rw.CostUSD, rw.Priced = cny, usd, true
			totalCNY += cny
			totalUSD += usd
			pricedModels++
			if _, src, _ := u.Pricing.Price(g.Key); src != "" {
				rw.Source = src
			}
		} else {
			unpriced++
		}
		out = append(out, rw)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"since": f.Since, "until": f.Until, "models": out,
		"totals": map[string]any{"cost_cny": totalCNY, "cost_usd": totalUSD, "priced_models": pricedModels, "unpriced_models": unpriced},
	})
}
