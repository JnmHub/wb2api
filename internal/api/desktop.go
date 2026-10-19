package api

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"wb2api/internal/realm"
)

// Desktop credentials: files the WorkBuddy desktop client stores under
// %LOCALAPPDATA%\CodeBuddyExtension\Data\Public\auth. Reading them is
// read-only; importing only happens on an explicit request.

func (o *Operations) desktopGet(w http.ResponseWriter, r *http.Request) {
	if o.AtRest == nil {
		writeErr(w, http.StatusServiceUnavailable, "桌面凭据导入不可用")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": o.AtRest.Status(), "files": o.AtRest.Scan()})
}

func (o *Operations) desktopPost(w http.ResponseWriter, r *http.Request) {
	if o.AtRest == nil {
		writeErr(w, http.StatusServiceUnavailable, "桌面凭据导入不可用")
		return
	}
	var in struct {
		KeyText string `json:"keyText"`
		Path    string `json:"path"`
		All     bool   `json:"all"`
		Forget  bool   `json:"forgetKey"`
		Realm   string `json:"realm"`
	}
	if r.ContentLength != 0 && !decode(w, r, &in) {
		return
	}
	if in.Forget {
		o.AtRest.Forget()
	}
	if strings.TrimSpace(in.KeyText) != "" {
		if _, err := o.AtRest.Adopt(in.KeyText); err != nil {
			writeErr(w, http.StatusBadRequest, "密钥校验失败: "+err.Error())
			return
		}
	}
	key := o.AtRest.Cached()
	imported := []map[string]any{}
	importOne := func(path, hint string) map[string]any {
		res := map[string]any{"path": path}
		if key == nil {
			res["error"] = "凭据已加密：先导入 at-rest 密钥"
			return res
		}
		row, err := o.AtRest.Credential(path, hint, key)
		if err != nil {
			res["error"] = err.Error()
			return res
		}
		acc, err := o.Ops.AddAccount(row, true)
		if err != nil {
			res["error"] = err.Error()
			return res
		}
		rec := acc.Record()
		res["uid"] = rec.UID
		res["nickname"] = rec.Nickname
		res["realm"] = rec.Realm
		res["ok"] = true
		if rec.Realm == realm.CN {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
			defer cancel()
			o.Ops.Checkin(ctx, acc, "account_import")
		}
		return res
	}
	switch {
	case strings.TrimSpace(in.Path) != "":
		hint := in.Realm
		if hint == "" {
			hint = realmHintOf(in.Path)
		}
		imported = append(imported, importOne(in.Path, hint))
	case in.All:
		for _, f := range o.AtRest.Scan() {
			if !f.Valid {
				continue
			}
			imported = append(imported, importOne(f.Path, f.Realm))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": o.AtRest.Status(), "files": o.AtRest.Scan(), "imported": imported})
}

func realmHintOf(path string) string {
	base := strings.ToLower(filepath.Base(path))
	if strings.Contains(base, "-ai.info") {
		return realm.Intl
	}
	return realm.CN
}
