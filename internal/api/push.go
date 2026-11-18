package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"wb2api/internal/ops"
)

// pushAccounts adds accounts from a remote caller using a gateway API key
// instead of the panel session.
//
// The panel's /api/v2/accounts/import needs a browser session and a CSRF
// header, which makes it useless for automation (a login robot, another
// gateway, a cron job). This endpoint accepts the same documents with a key:
//
//	POST /api/v2/accounts/push
//	Authorization: Bearer <api key>
//	{"accounts": [ {"uid": "...", "realm": "codearts", "accessToken": "..."} ]}
//
// A bare array, a single account object, an export bundle ({"document": ...})
// and the raw panel export file all work. Accounts are upserted: an existing
// uid is overwritten, which is what a re-login robot needs.
func (a *Accounts) pushAccounts(w http.ResponseWriter, r *http.Request) {
	if a.Ops == nil || a.Store == nil {
		writeErr(w, http.StatusServiceUnavailable, "account store is not configured")
		return
	}
	if !a.pushAuthorized(r) {
		writeErr(w, http.StatusUnauthorized, "a valid API key is required (Authorization: Bearer <key>)")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "cannot read the request body")
		return
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		writeErr(w, http.StatusBadRequest, "the request body is empty")
		return
	}
	doc, err := pushDocument(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	rows, err := ops.ParseDocument(doc)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(rows) == 0 {
		writeErr(w, http.StatusBadRequest, "no account rows found in the body")
		return
	}
	if len(rows) > 5000 {
		writeErr(w, http.StatusBadRequest, "at most 5000 accounts per push")
		return
	}
	dry := strings.EqualFold(r.URL.Query().Get("dry_run"), "1") || strings.EqualFold(r.URL.Query().Get("dry_run"), "true")
	rep := a.Ops.Import(rows, "", true, dry)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "count": len(rows), "dry_run": dry, "result": rep,
	})
}

// pushAuthorized accepts any enabled API key, mirroring the gateway's own rule:
// when key auth is switched off, the endpoint is open like the rest is.
func (a *Accounts) pushAuthorized(r *http.Request) bool {
	keys := a.Store.APIKeys(false)
	tok := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if tok == "" {
		tok = strings.TrimSpace(r.Header.Get("x-api-key"))
	}
	if tok != "" {
		for _, k := range keys {
			if k.Key != "" && k.Key == tok {
				return true
			}
		}
		return false
	}
	if a.Store.AuthDisabled() {
		return true
	}
	// No key configured yet: a write endpoint stays closed to the outside and
	// open only to the machine running the gateway, so a fresh install is not
	// silently writable from the internet.
	return len(keys) == 0 && isLoopback(r)
}

// pushDocument normalises the accepted shapes into one {"accounts": [...]}
// document the existing importer already understands.
func pushDocument(raw []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		return append(append([]byte(`{"accounts":`), trimmed...), '}'), nil
	}
	var obj map[string]any
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return nil, err
	}
	if inner, ok := obj["document"]; ok {
		return json.Marshal(inner)
	}
	if _, ok := obj["accounts"]; ok {
		return trimmed, nil
	}
	if _, ok := obj["uid"]; ok {
		return json.Marshal(map[string]any{"accounts": []any{obj}})
	}
	return nil, errNoAccountsInBody
}

type pushError string

func (e pushError) Error() string { return string(e) }

const errNoAccountsInBody = pushError("expected an account object, an array of accounts or {\"accounts\": [...]}")

// PushHandler exposes the API-key account push to the operations router.
func (a *Accounts) PushHandler() http.HandlerFunc { return a.pushAccounts }
