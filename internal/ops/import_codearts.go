package ops

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"wb2api/internal/realm"
	"wb2api/internal/upstream/codearts"
)

func isCodeArtsRow(row Row) bool {
	if str(row["realm"]) == realm.CodeArts || str(row["provider"]) == realm.CodeArts {
		return true
	}
	for _, body := range []Row{row, asRow(row["extra"])} {
		if asRow(body["codearts"]) != nil || asRow(body["codearts_provider_credential"]) != nil {
			return true
		}
	}
	for _, body := range []Row{row, asRow(row["credential"]), asRow(row["credentials"])} {
		for _, key := range []string{"cloud_dragon_token", "security_token", "access_key_id"} {
			if text(body[key]) != "" {
				return true
			}
		}
	}
	return false
}

func normaliseCodeArtsRow(row Row) (Row, error) {
	extra := asRow(row["extra"])
	native := asRow(row["codearts"])
	if native == nil {
		native = asRow(extra["codearts"])
	}
	agent := asRow(row["codearts_provider_credential"])
	if agent == nil {
		agent = asRow(extra["codearts_provider_credential"])
	}
	layers := []Row{agent, asRow(row["credential"]), asRow(row["credentials"]), native, row, extra}
	pick := func(keys ...string) string {
		for _, layer := range layers {
			for _, k := range keys {
				if v := text(layer[k]); v != "" {
					return v
				}
			}
		}
		return ""
	}
	agentExport := str(row["provider"]) == realm.CodeArts || agent != nil || pick("login_type") != ""
	uid := strings.Trim(uidClean.ReplaceAllString(pick("user_id", "userId", "uid", "principal_id"), "_"), "_ ")
	token := pick("security_token", "cloud_dragon_token", "securityToken", "securitytoken")
	if token == "" && !agentExport {
		token = pick("access_token", "accessToken")
	}
	ak := pick("access_key_id", "accessKeyId", "access_key", "access")
	sk := pick("secret_access_key", "secretAccessKey", "secret_key", "secret")
	switch {
	case uid == "":
		return nil, fmt.Errorf("缺少 uid / user_id")
	case token == "":
		return nil, fmt.Errorf("缺少 security_token / cloud_dragon_token")
	case ak == "" || sk == "":
		return nil, fmt.Errorf("缺少 access_key_id / secret_access_key")
	}
	exp := codeArtsImportExpiry(layers)
	var dpop Row
	verifier := pick("code_verifier", "codeVerifier")
	clientID := pick("client_id", "clientId")
	for _, layer := range layers {
		oauth := asRow(layer["oauth_context"])
		if oauth == nil {
			oauth = asRow(layer["oauthContext"])
		}
		if len(dpop) == 0 {
			dpop = asRow(layer["dpop_private_key"])
			if len(dpop) == 0 {
				dpop = asRow(asRow(oauth["dpop_key_pair"])["private_key_jwk"])
			}
		}
		if verifier == "" {
			verifier = text(firstStr(asRow(oauth["pkce_pair"])["code_verifier"], oauth["code_verifier"]))
		}
		if clientID == "" {
			clientID = text(firstStr(oauth["client_id"], oauth["clientId"]))
		}
	}
	rt := pick("refresh_token", "refreshToken")
	if clientID == "" {
		clientID = codearts.ClientID
		if agentExport {
			// Agent2Providers v3.4.2 does not persist its compiled-in ID.
			clientID = "vscode-codebot"
		}
	}
	if agentExport {
		switch {
		case exp <= 0:
			return nil, fmt.Errorf("Agent2API 凭据缺少有效到期时间")
		case rt == "" || verifier == "" || len(dpop) == 0:
			return nil, fmt.Errorf("Agent2API 凭据缺少 refresh token、PKCE verifier 或 DPoP 私钥")
		}
		key := make(codearts.DPoPKey, len(dpop))
		for k, v := range dpop {
			key[k], _ = v.(string)
		}
		if err := codearts.ValidateDPoPKey(key); err != nil {
			return nil, fmt.Errorf("Agent2API 凭据的 DPoP 私钥无效")
		}
	}
	meta := Row{}
	for k, v := range native {
		meta[k] = v
	}
	meta["access_key_id"], meta["secret_access_key"], meta["security_token"] = ak, sk, token
	meta["expiration"], meta["client_id"] = pick("expiration", "expires_at"), clientID
	meta["code_verifier"], meta["dpop_private_key"] = verifier, dpop
	meta["domain_id"], meta["user_id"] = pick("domain_id", "domainId", "account_id"), uid
	result := Row{
		"uid": uid, "nickname": firstStr(firstStr(row["nickname"], row["name"]), pick("user_name", "userName")),
		"realm": realm.CodeArts, "accessToken": token, "refreshToken": rt, "expiresAt": exp,
		"source": "codearts-import", "platform": "codearts", "codearts": meta,
	}
	for _, key := range []string{"enabled", "note", "weight", "proxySlot", "tasksEnabled", "lastCheckin",
		"lastDailyChat", "lastActivityReport", "credits", "disabledModels", "restrictedUntil", "modelCaps",
		"modelCool", "modelCoolWhy", "addedAt", "tier", "codeartsAuth"} {
		if v, ok := row[key]; ok {
			result[key] = v
		}
	}
	switch proxy := row["proxy"].(type) {
	case string:
		result["proxy"] = proxy
	case map[string]any:
		if proxy["enabled"] == true {
			result["proxy"] = text(proxy["url"])
		}
	}
	return result, nil
}

func codeArtsImportExpiry(layers []Row) int64 {
	for _, layer := range layers {
		for _, field := range []string{"expiration", "expires_at", "expiresAt"} {
			if ts, err := time.Parse(time.RFC3339Nano, text(layer[field])); err == nil {
				return ts.Unix()
			}
			if exp := realm.NormalizeEpoch(layer[field]); exp > 0 {
				return exp
			}
		}
	}
	return 0
}

func asRow(v any) Row {
	m, _ := v.(map[string]any)
	return m
}

func text(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func removeCodeArtsSecrets(m map[string]json.RawMessage) {
	for _, key := range []string{"codearts", "codearts_provider_credential", "credential", "credentials",
		"access_key_id", "accessKeyId", "access_key", "secret_access_key", "secretAccessKey",
		"secret_key", "security_token", "securityToken", "cloud_dragon_token",
		"refresh_token", "access_token", "dpop_private_key", "code_verifier", "codeVerifier",
		"oauth_context", "oauthContext"} {
		delete(m, key)
	}
}
