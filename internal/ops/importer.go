package ops

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"wb2api/internal/jsonx"
	"wb2api/internal/realm"
)

// Export document format.
const (
	ExportFormat  = "workbuddy-accounts"
	ExportVersion = 1
)

// Row is one account document being imported.
type Row = map[string]any

func decodeAny(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(raw)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// ParseDocument accepts this gateway's own export, a bare list, a single
// account object, or a desktop-app credential, and returns the account rows.
func ParseDocument(raw []byte) ([]Row, error) {
	v, err := decodeAny(raw)
	if err != nil {
		return nil, fmt.Errorf("not valid JSON: %v", err)
	}
	var rows []any
	switch t := v.(type) {
	case []any:
		rows = t
	case map[string]any:
		if list, ok := t["accounts"].([]any); ok {
			rows = list
		} else {
			_, hasAuth := t["auth"].(map[string]any)
			_, hasAcc := t["account"].(map[string]any)
			if !(jsonx.Truthy(t["accessToken"]) || hasAuth || hasAcc) {
				keys := make([]string, 0, len(t))
				for k := range t {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				if len(keys) > 6 {
					keys = keys[:6]
				}
				return nil, fmt.Errorf("not an account document (expected an accounts array, a list, or an account object; got keys: %s)", strings.Join(keys, ", "))
			}
			rows = []any{t}
		}
	default:
		return nil, fmt.Errorf("expected an object or a list of accounts")
	}
	out := make([]Row, 0, len(rows))
	for i, r := range rows {
		m, ok := r.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("account #%d is not an object", i+1)
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no accounts found in the document")
	}
	return out, nil
}

var uidClean = regexp.MustCompile(`[^A-Za-z0-9_-]`)

func str(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(t)
	case json.Number:
		return t.String()
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

// NormaliseRow turns one foreign row into a record map. It reads the flat shape,
// the nested desktop-credential shape ({auth, account}) and the snake_case
// shape other tools export. The exit comes from forced (when given), else from
// the token / domain, so a stale "realm" field from an account once filed under
// the wrong exit does not keep dragging it back there.
func NormaliseRow(row Row, forced string) (Row, error) {
	if _, hasSnake := row["access_token"]; hasSnake && row["accessToken"] == nil {
		row = Row{"uid": row["uid"], "nickname": firstStr(row["nickname"], row["email"]), "domain": row["domain"],
			"accessToken": row["access_token"], "refreshToken": row["refresh_token"], "expiresAt": row["expires_at"], "source": "cockpit"}
	}
	auth, _ := row["auth"].(map[string]any)
	prof, _ := row["account"].(map[string]any)
	pick := func(key string) any {
		for _, layer := range []map[string]any{row, auth, prof} {
			if layer != nil {
				if v := layer[key]; v != nil && v != "" {
					return v
				}
			}
		}
		return nil
	}
	token := str(pick("accessToken"))
	if token == "" {
		return nil, fmt.Errorf("no accessToken")
	}
	if strings.Count(token, ".") != 2 {
		return nil, fmt.Errorf("accessToken is not a JWT")
	}
	forced = strings.ToLower(strings.TrimSpace(forced))
	evidence := realm.Evidence(token, str(pick("domain")))
	rowRealm := strings.ToLower(str(pick("realm")))
	var detected string
	switch {
	case forced == realm.Intl || forced == realm.CN:
		detected = forced
	case evidence != "":
		detected = evidence
	case rowRealm == realm.Intl || rowRealm == realm.CN:
		detected = rowRealm
	default:
		detected = realm.Detect(token, str(pick("domain")))
	}
	uid := str(pick("uid"))
	if uid == "" {
		uid = realm.JWTUID(token)
	}
	uid = strings.Trim(uidClean.ReplaceAllString(uid, "_"), "_ ")
	if uid == "" {
		return nil, fmt.Errorf("cannot determine uid (no uid field and no sub claim)")
	}
	exp := realm.NormalizeEpoch(pick("expiresAt"))
	if exp == 0 {
		exp = realm.JWTExp(token)
	}
	platform := str(pick("platform"))
	if platform == "" {
		platform = "CLI"
	}
	source := str(pick("source"))
	if source == "" {
		source = "import"
	}
	return Row{
		"uid": uid, "nickname": str(pick("nickname")), "domain": realm.DomainFor(detected, str(pick("domain"))),
		"realm": detected, "platform": platform, "enterpriseId": str(pick("enterpriseId")), "accessToken": token,
		"refreshToken": str(pick("refreshToken")), "expiresAt": exp, "source": source, "enabled": true,
	}, nil
}

func firstStr(a, b any) any {
	if str(a) != "" {
		return a
	}
	return b
}

// ImportReport says what an import did (or, in a dry run, would do).
type ImportReport struct {
	Added   []string         `json:"added"`
	Updated []string         `json:"updated"`
	Skipped []map[string]any `json:"skipped"`
	Invalid []map[string]any `json:"invalid"`
}

// Import adds accounts from rows. With dryRun nothing is touched: the report
// uses the same accept/skip rules, so a preview cannot disagree with the real
// thing. One bad row never aborts the rest.
func (c *Client) Import(rows []Row, forcedRealm string, overwrite, dryRun bool) ImportReport {
	rep := ImportReport{Added: []string{}, Updated: []string{}, Skipped: []map[string]any{}, Invalid: []map[string]any{}}
	seen := map[string]bool{}
	for i, row := range rows {
		norm, err := NormaliseRow(row, forcedRealm)
		if err != nil {
			rep.Invalid = append(rep.Invalid, map[string]any{"index": i + 1, "reason": err.Error()})
			continue
		}
		uid := norm["uid"].(string)
		if seen[uid] {
			rep.Skipped = append(rep.Skipped, map[string]any{"uid": uid, "reason": "duplicate inside the document"})
			continue
		}
		seen[uid] = true
		exists := c.Pool.Get(uid) != nil
		if exists && !overwrite {
			rep.Skipped = append(rep.Skipped, map[string]any{"uid": uid, "reason": "already exists"})
			continue
		}
		if !dryRun {
			if _, err := c.AddAccount(norm, true); err != nil {
				rep.Invalid = append(rep.Invalid, map[string]any{"index": i + 1, "reason": err.Error()})
				continue
			}
		}
		if exists {
			rep.Updated = append(rep.Updated, uid)
		} else {
			rep.Added = append(rep.Added, uid)
		}
	}
	return rep
}

// Export builds a self-describing export document. uids narrows it (nil = all);
// realmName filters by exit; without secrets the tokens are left out.
func (c *Client) Export(realmName string, uids []string, secrets bool) (map[string]any, error) {
	want := map[string]bool{}
	for _, u := range uids {
		want[u] = true
	}
	var rows []json.RawMessage
	for _, a := range c.Pool.All() {
		rec := a.Record()
		if (realmName != "" && rec.Realm != realmName) || (len(want) > 0 && !want[rec.UID]) {
			continue
		}
		b, err := rec.Marshal()
		if err != nil {
			return nil, err
		}
		if !secrets {
			var m map[string]json.RawMessage
			if json.Unmarshal(b, &m) == nil {
				delete(m, "accessToken")
				delete(m, "refreshToken")
				b, _ = json.Marshal(m)
			}
		}
		rows = append(rows, b)
	}
	if rows == nil {
		rows = []json.RawMessage{}
	}
	return map[string]any{
		"format": ExportFormat, "version": ExportVersion, "exportedAt": stampNow(c.now()),
		"count": len(rows), "accounts": rows,
	}, nil
}
