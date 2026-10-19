package api

import (
	"net"
	"net/http"
	"strings"
)

func (o *Operations) agentsGet(w http.ResponseWriter, r *http.Request) {
	if o.Agents == nil {
		writeErr(w, http.StatusServiceUnavailable, "智能体配置不可用")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"clients": o.Agents.Overview(), "loopback": isLoopback(r), "base_url": "http://" + r.Host,
	})
}

type agentApplyReq struct {
	Client  string   `json:"client"`
	BaseURL string   `json:"base_url"`
	APIKey  string   `json:"api_key"`
	Model   string   `json:"model"`
	Models  []string `json:"models"`
}

func (o *Operations) agentsApply(w http.ResponseWriter, r *http.Request) {
	if o.Agents == nil {
		writeErr(w, http.StatusServiceUnavailable, "智能体配置不可用")
		return
	}
	if !isLoopback(r) {
		writeErr(w, http.StatusForbidden, "智能体配置只允许从本机操作")
		return
	}
	var in agentApplyReq
	if !decode(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.APIKey) == "" {
		writeErr(w, http.StatusBadRequest, "api_key is required")
		return
	}
	if strings.TrimSpace(in.BaseURL) == "" {
		in.BaseURL = "http://" + r.Host
	}
	res, err := o.Agents.Apply(in.Client, in.BaseURL, in.APIKey, in.Model, in.Models)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": res, "clients": o.Agents.Overview()})
}

func (o *Operations) agentsRestore(w http.ResponseWriter, r *http.Request) {
	if o.Agents == nil {
		writeErr(w, http.StatusServiceUnavailable, "智能体配置不可用")
		return
	}
	if !isLoopback(r) {
		writeErr(w, http.StatusForbidden, "智能体配置只允许从本机操作")
		return
	}
	var in struct {
		Client string `json:"client"`
	}
	if !decode(w, r, &in) {
		return
	}
	res, err := o.Agents.Restore(in.Client)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": res, "clients": o.Agents.Overview()})
}

func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
