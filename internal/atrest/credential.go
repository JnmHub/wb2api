package atrest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"wb2api/internal/realm"
)

// Credential is one account row recovered from a desktop credential file.
type Credential struct {
	Path      string `json:"path"`
	File      string `json:"file"`
	Realm     string `json:"realm"`
	RealmName string `json:"realm_name"`
	Domain    string `json:"domain"`
	UID       string `json:"uid"`
	Nickname  string `json:"nickname"`
	ExpiresAt int64  `json:"expires_at"`
	Readable  bool   `json:"readable"`
	Valid     bool   `json:"valid"`
	Encrypted bool   `json:"encrypted"`
	NeedsKey  bool   `json:"needs_key"`
	Error     string `json:"error"`
}

type candidate struct {
	path  string
	realm string
}

type credDocument struct {
	Auth struct {
		AccessToken  any    `json:"accessToken"`
		RefreshToken any    `json:"refreshToken"`
		ExpiresAt    any    `json:"expiresAt"`
		Domain       string `json:"domain"`
	} `json:"auth"`
	Account struct {
		UID          string `json:"uid"`
		Nickname     any    `json:"nickname"`
		EnterpriseID string `json:"enterpriseId"`
	} `json:"account"`
}

// AuthDirs lists the directories the desktop client keeps its *.info files in.
func AuthDirs() []string {
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		return []string{filepath.Join(local, "CodeBuddyExtension", "Data", "Public", "auth")}
	}
	home, _ := os.UserHomeDir()
	base := filepath.Join(home, ".local", "share")
	if os.PathSeparator == '\\' {
		base = filepath.Join(home, "AppData", "Local")
	}
	var out []string
	for _, app := range []string{"CodeBuddyExtension", "WorkBuddy", "CodeBuddy"} {
		out = append(out, filepath.Join(base, app, "Data", "Public", "auth"))
		out = append(out, filepath.Join(base, app, "auth"))
	}
	return out
}

func candidates() []candidate {
	var out []candidate
	for _, dir := range AuthDirs() {
		for _, item := range []struct{ name, realm string }{
			{"workbuddy-desktop-ai.info", realm.Intl},
			{"workbuddy-desktop.info", realm.CN},
		} {
			p := filepath.Join(dir, item.name)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				out = append(out, candidate{p, item.realm})
			}
		}
	}
	return out
}

// Scan describes the credential files found on this machine. It is read-only
// and only uses a key that is already cached, so it never blocks on a hunt.
func (k *Keyring) Scan() []Credential {
	key := k.Cached()
	out := []Credential{}
	for _, c := range candidates() {
		item := Credential{Path: c.path, File: filepath.Base(c.path), Realm: c.realm, RealmName: realmName(c.realm),
			Domain: realm.Get(c.realm).Domain}
		raw, err := os.ReadFile(c.path)
		if err != nil {
			item.Error = err.Error()
			out = append(out, item)
			continue
		}
		item.Readable = true
		var doc credDocument
		if err := json.Unmarshal(raw, &doc); err != nil {
			item.Error = "不是有效的桌面凭据 JSON"
			out = append(out, item)
			continue
		}
		rawToken := doc.Auth.AccessToken
		var token string
		if IsEnvelope(rawToken) {
			item.Encrypted = true
			if key == nil {
				item.NeedsKey = true
				item.Error = "凭据已加密：先导入 at-rest 密钥，再回来导入"
				out = append(out, item)
				continue
			}
			token, _ = DecryptField(key, rawToken)
		} else if s, ok := rawToken.(string); ok {
			token = s
		}
		if token == "" {
			item.Error = "文件里没有 accessToken"
			out = append(out, item)
			continue
		}
		nick, _ := DecryptField(key, doc.Account.Nickname)
		item.UID = doc.Account.UID
		if item.UID == "" {
			item.UID = realm.JWTUID(token)
		}
		item.Nickname = nick
		item.ExpiresAt = realm.NormalizeEpoch(doc.Auth.ExpiresAt)
		if item.ExpiresAt == 0 {
			item.ExpiresAt = realm.JWTExp(token)
		}
		if evidence := realm.Evidence(token, doc.Auth.Domain); evidence != "" {
			item.Realm = evidence
			item.RealmName = realmName(evidence)
			item.Domain = realm.DomainFor(evidence, doc.Auth.Domain)
		}
		item.Valid = item.UID != ""
		if !item.Valid {
			item.Error = "无法确定账号 UID"
		}
		out = append(out, item)
	}
	return out
}

// Credential decrypts one file into an account row suitable for the pool.
func (k *Keyring) Credential(path, realmHint string, key []byte) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc credDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("不是有效的桌面凭据 JSON：%w", err)
	}
	token := ""
	if IsEnvelope(doc.Auth.AccessToken) {
		if key == nil {
			return nil, fmt.Errorf("凭据已加密：先导入 at-rest 密钥")
		}
		token, _ = DecryptField(key, doc.Auth.AccessToken)
	} else if s, ok := doc.Auth.AccessToken.(string); ok {
		token = s
	}
	if token == "" {
		return nil, fmt.Errorf("no accessToken in %s", path)
	}
	nickname, _ := DecryptField(key, doc.Account.Nickname)
	refresh := ""
	if IsEnvelope(doc.Auth.RefreshToken) {
		if key != nil {
			refresh, _ = DecryptField(key, doc.Auth.RefreshToken)
		}
	} else if s, ok := doc.Auth.RefreshToken.(string); ok {
		refresh = s
	}
	detected := realm.Evidence(token, doc.Auth.Domain)
	if detected == "" {
		detected = realmHint
	}
	if detected != realm.Intl && detected != realm.CN {
		detected = realm.Intl
	}
	uid := doc.Account.UID
	if uid == "" {
		uid = realm.JWTUID(token)
	}
	exp := realm.NormalizeEpoch(doc.Auth.ExpiresAt)
	if exp == 0 {
		exp = realm.JWTExp(token)
	}
	return map[string]any{
		"uid": uid, "nickname": nickname, "domain": realm.DomainFor(detected, doc.Auth.Domain),
		"realm": detected, "platform": "CLI", "enterpriseId": doc.Account.EnterpriseID,
		"accessToken": token, "refreshToken": refresh, "expiresAt": exp,
		"source": "desktop-app", "enabled": true,
	}, nil
}

func realmName(r string) string {
	if r == realm.CN {
		return "国内版"
	}
	return "国际版"
}

// IsCredentialPath reports a path that looks like a desktop credential file.
func IsCredentialPath(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	return strings.HasSuffix(base, ".info") && strings.Contains(base, "workbuddy")
}
