package ops

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"sort"
	"strings"

	"wb2api/internal/jsonx"
	"wb2api/internal/realm"
	"wb2api/internal/upstream/raccoon"
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
			if !(jsonx.Truthy(t["accessToken"]) || jsonx.Truthy(t["access_token"]) || hasAuth || hasAcc || isCodeArtsRow(t) || isRaccoonRow(t)) {
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
	forced = strings.ToLower(strings.TrimSpace(forced))
	// The Huawei channel has its own credential shape: an opaque STS token, an
	// AK/SK pair and a DPoP key. It never passes the JWT checks below.
	if isCodeArtsRow(row) {
		return normaliseCodeArtsRow(row)
	}
	if isRaccoonRow(row) || forced == realm.Raccoon {
		return normaliseRaccoonRow(row)
	}
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

func isRaccoonRow(row Row) bool {
	if strings.EqualFold(str(row["realm"]), realm.Raccoon) || strings.EqualFold(str(row["provider"]), realm.Raccoon) {
		return true
	}
	if asRow(row[raccoon.ExtraKey]) != nil || asRow(asRow(row["extra"])[raccoon.ExtraKey]) != nil {
		return true
	}
	return strings.EqualFold(raccoon.Issuer(str(firstStr(row["accessToken"], row["access_token"]))), raccoon.ExpectedIAMIssuer())
}

func normaliseRaccoonRow(row Row) (Row, error) {
	extra := asRow(row["extra"])
	fields := maps.Clone(asRow(row[raccoon.ExtraKey]))
	if fields == nil {
		fields = maps.Clone(asRow(extra[raccoon.ExtraKey]))
	}
	if fields == nil {
		fields = make(Row)
	}
	pick := func(keys ...string) string {
		for _, layer := range []Row{row, asRow(row["auth"]), fields, extra} {
			for _, key := range keys {
				if value := str(layer[key]); value != "" {
					return value
				}
			}
		}
		return ""
	}
	token := raccoon.StripBearer(pick("accessToken", "access_token", "token"))
	if token == "" {
		return nil, fmt.Errorf("missing Raccoon accessToken")
	}
	if strings.Count(token, ".") != 2 || strings.ContainsAny(token, " \t\r\n\"{}") || raccoon.Claims(token) == nil {
		return nil, fmt.Errorf("Raccoon accessToken is not a JWT")
	}
	if realm.Evidence(token, "") != "" {
		return nil, fmt.Errorf("Raccoon account carries a WorkBuddy token")
	}
	if raccoon.IsIAM(token) && !strings.EqualFold(raccoon.Issuer(token), raccoon.ExpectedIAMIssuer()) {
		return nil, fmt.Errorf("invalid Raccoon IAM token issuer")
	}
	uid := str(row["uid"])
	if uid == "" {
		id := pick("userId", "user_id")
		if id == "" {
			id = raccoon.UserID(token)
		}
		if id == "" || id == "unknown" {
			return nil, fmt.Errorf("cannot determine Raccoon user ID")
		}
		uid = "rc-" + id
	}
	if strings.Trim(uidClean.ReplaceAllString(uid, "_"), "_ ") != uid {
		return nil, fmt.Errorf("invalid Raccoon account UID")
	}
	out := maps.Clone(row)
	// Older exporters may group private fields under extra; account files use
	// top-level provider data. Preserve other metadata in either layout.
	for key, value := range extra {
		if _, exists := out[key]; !exists {
			out[key] = value
		}
	}
	out["uid"], out["realm"], out["platform"], out["accessToken"] = uid, realm.Raccoon, realm.Raccoon, token
	refresh := raccoon.StripBearer(pick("refreshToken", "refresh_token"))
	if raccoon.IsIAM(token) {
		refresh = ""
	}
	out["refreshToken"] = refresh
	expiry := realm.NormalizeEpoch(pick("expiresAt", "expires_at"))
	if expiry <= 0 {
		expiry = realm.JWTExp(token)
	}
	out["expiresAt"] = expiry
	if str(out["source"]) == "" {
		out["source"] = "raccoon-import"
	}
	for _, key := range []string{"officeIdentity", "office_identity", "officeOrgName", "office_org_name", "officeOrgRole", "office_org_role"} {
		if value := pick(key); value != "" {
			fields[key] = value
		}
	}
	for _, key := range []string{"token", "access_token", "refresh_token"} {
		delete(out, key)
		delete(fields, key)
	}
	delete(fields, "accessToken")
	delete(fields, "refreshToken")
	delete(out, "extra")
	delete(out, "auth")
	out[raccoon.ExtraKey] = fields
	return out, nil
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
		current := c.Pool.Get(uid)
		exists := current != nil
		if exists && !overwrite {
			rep.Skipped = append(rep.Skipped, map[string]any{"uid": uid, "reason": "already exists"})
			continue
		}
		if exists && current.Realm != norm["realm"] && (realm.IsRaccoon(current.Realm) || norm["realm"] == realm.Raccoon) {
			rep.Invalid = append(rep.Invalid, map[string]any{"index": i + 1, "reason": "UID belongs to another realm"})
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
				// The Huawei channel keeps its AK/SK chain in the record extra:
				// a secret-free export must not leak it.
				if realm.IsCodeArts(rec.Realm) {
					removeCodeArtsSecrets(m)
				} else if realm.IsRaccoon(rec.Realm) {
					removeRaccoonSecrets(m)
				}
				if raw, ok := m["extra"]; ok {
					var extra map[string]json.RawMessage
					if json.Unmarshal(raw, &extra) == nil {
						if realm.IsCodeArts(rec.Realm) {
							removeCodeArtsSecrets(extra)
						}
						if nb, err := json.Marshal(extra); err == nil {
							m["extra"] = nb
						}
					}
				}
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

func removeRaccoonSecrets(fields map[string]json.RawMessage) {
	for _, key := range []string{"token", "accessToken", "access_token", "refreshToken", "refresh_token"} {
		delete(fields, key)
	}
	for key, value := range fields {
		if len(value) == 0 || value[0] != '{' {
			continue
		}
		var nested map[string]json.RawMessage
		if json.Unmarshal(value, &nested) == nil {
			removeRaccoonSecrets(nested)
			fields[key], _ = json.Marshal(nested)
		}
	}
}
