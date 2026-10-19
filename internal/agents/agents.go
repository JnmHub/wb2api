// Package agents points installed AI CLIs at this gateway. It is a port of the
// config-writing half of legacy wb_agents.py: detect Claude Code, Codex,
// OpenCode, DSH, Crush and ZCode; back up every touched file; apply the
// provider block; restore the pre-integration bytes on request.
package agents

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"
)

const (
	ProviderID    = "wb-proxy"
	ProviderName  = "WB Proxy"
	CredentialRef = "WB_PROXY_API_KEY"
	backupKeep    = 10
)

// Applied is the state record of one client integration.
type Applied struct {
	At       string `json:"at"`
	BaseURL  string `json:"base_url"`
	Model    string `json:"model"`
	Files    int    `json:"files"`
	External bool   `json:"external_change"`
}

// ClientView is one row of GET /agents.
type ClientView struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	Desc        string   `json:"desc"`
	Protocol    string   `json:"protocol"`
	Installed   bool     `json:"installed"`
	Configured  bool     `json:"configured"`
	ConfigPaths []string `json:"config_paths"`
	Applied     *Applied `json:"applied,omitempty"`
}

type fileEntry struct {
	Path    string `json:"path"`
	Existed bool   `json:"existed"`
	Backup  string `json:"backup,omitempty"`
	SHA256  string `json:"sha256"`
	Bytes   int    `json:"bytes"`
}

type stateRecord struct {
	AppliedAt string      `json:"applied_at"`
	BaseURL   string      `json:"base_url"`
	Model     string      `json:"model"`
	Files     []fileEntry `json:"files"`
}

type stateDoc struct {
	Clients map[string]stateRecord `json:"clients"`
}

// Manager reads and writes the clients' config files under one home directory.
type Manager struct {
	AccountsDir string
	Home        string
	Now         func() time.Time
}

// New resolves the home directory once (tests set Manager.Home directly).
func New(accountsDir string) *Manager {
	home, _ := os.UserHomeDir()
	return &Manager{AccountsDir: accountsDir, Home: home, Now: time.Now}
}

type clientDef struct {
	id, label, desc, protocol string
	paths                     func(home string) map[string]string
	probe                     func(home string) bool
	apply                     func(m *Manager, paths map[string]string, ctx applyCtx) (map[string][]byte, error)
}

type applyCtx struct {
	BaseURL   string
	BaseURLV1 string
	APIKey    string
	Model     string
	Models    []string
}

func claudePath(home string) string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "settings.json")
	}
	return filepath.Join(home, ".claude", "settings.json")
}

func codexHome(home string) string {
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		return dir
	}
	return filepath.Join(home, ".codex")
}

var clients = []clientDef{
	{id: "claude-code", label: "Claude Code", desc: "Anthropic CLI；把 ANTHROPIC_BASE_URL 指向本网关。", protocol: "anthropic",
		paths: func(home string) map[string]string { return map[string]string{"settings": claudePath(home)} },
		probe: func(home string) bool {
			dir := os.Getenv("CLAUDE_CONFIG_DIR")
			if dir == "" {
				dir = filepath.Join(home, ".claude")
			}
			return isDir(dir) || fileExists(claudePath(home)) || hasBin("claude")
		},
		apply: applyClaude},
	{id: "codex", label: "Codex CLI", desc: "OpenAI Codex CLI；新增 wb-proxy provider。", protocol: "openai",
		paths: func(home string) map[string]string {
			return map[string]string{"config": filepath.Join(codexHome(home), "config.toml"), "auth": filepath.Join(codexHome(home), "auth.json")}
		},
		probe: func(home string) bool {
			return isDir(codexHome(home)) || fileExists(filepath.Join(codexHome(home), "config.toml")) || hasBin("codex")
		},
		apply: applyCodex},
	{id: "opencode", label: "OpenCode", desc: "OpenCode；注册一个 openai 兼容 provider。", protocol: "openai",
		paths: func(home string) map[string]string {
			return map[string]string{"config": filepath.Join(home, ".config", "opencode", "opencode.json")}
		},
		probe: func(home string) bool {
			return isDir(filepath.Join(home, ".config", "opencode")) || hasBin("opencode")
		},
		apply: applyOpenCode},
	{id: "dsh", label: "DeepSeek Harness", desc: "DSH；往 settings.yaml 加 wb-proxy provider。", protocol: "openai",
		paths: func(home string) map[string]string {
			return map[string]string{
				"settings":    filepath.Join(home, ".dsh", "settings.yaml"),
				"credentials": filepath.Join(home, ".dsh", ".credentials.yaml"),
			}
		},
		probe: func(home string) bool {
			return isDir(filepath.Join(home, ".dsh")) || hasBin("dsh")
		},
		apply: applyDSH},
	{id: "crush", label: "Crush", desc: "Charm Crush；新增 wb-proxy provider。", protocol: "openai",
		paths: func(home string) map[string]string {
			return map[string]string{"config": filepath.Join(home, ".config", "crush", "crush.json")}
		},
		probe: func(home string) bool {
			return isDir(filepath.Join(home, ".config", "crush")) || hasBin("crush")
		},
		apply: applyCrush},
	{id: "zcode", label: "ZCode", desc: "ZCode；往 provider 配置加规则。配置前请先退出 ZCode。", protocol: "openai",
		paths: func(home string) map[string]string {
			return map[string]string{"config": filepath.Join(home, ".zcode", "v2", "provider_config.json")}
		},
		probe: func(home string) bool { return isDir(filepath.Join(home, ".zcode")) },
		apply: applyZCode},
}

func isDir(p string) bool      { st, err := os.Stat(p); return err == nil && st.IsDir() }
func fileExists(p string) bool { st, err := os.Stat(p); return err == nil && !st.IsDir() }
func hasBin(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func (m *Manager) client(id string) (*clientDef, error) {
	for i := range clients {
		if clients[i].id == id {
			return &clients[i], nil
		}
	}
	return nil, fmt.Errorf("unknown client %q", id)
}

func (m *Manager) statePath() string { return filepath.Join(m.AccountsDir, "agents.json") }

func (m *Manager) loadState() stateDoc {
	doc := stateDoc{Clients: map[string]stateRecord{}}
	raw, err := os.ReadFile(m.statePath())
	if err == nil {
		_ = json.Unmarshal(raw, &doc)
		if doc.Clients == nil {
			doc.Clients = map[string]stateRecord{}
		}
	}
	return doc
}

func (m *Manager) saveState(doc stateDoc) error {
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(m.statePath(), raw, 0o600)
}

// Overview lists every client with its install/integration state.
func (m *Manager) Overview() []ClientView {
	state := m.loadState()
	out := make([]ClientView, 0, len(clients))
	for _, c := range clients {
		paths := c.paths(m.Home)
		installed := c.probe(m.Home)
		var list []string
		for _, p := range paths {
			list = append(list, p)
			if fileExists(p) {
				installed = true
			}
		}
		sort.Strings(list)
		v := ClientView{ID: c.id, Label: c.label, Desc: c.desc, Protocol: c.protocol,
			Installed: installed, ConfigPaths: list}
		if rec, ok := state.Clients[c.id]; ok {
			external := false
			for _, f := range rec.Files {
				raw, err := os.ReadFile(f.Path)
				if err != nil {
					external = true
					continue
				}
				// Compare with the bytes we wrote, not the backup: a hand edit
				// after an apply must read as an external change.
				if hashBytes(raw) != f.SHA256 {
					external = true
				}
			}
			v.Configured = true
			v.Applied = &Applied{At: rec.AppliedAt, BaseURL: rec.BaseURL, Model: rec.Model,
				Files: len(rec.Files), External: external}
		}
		out = append(out, v)
	}
	return out
}

// Apply writes the provider configuration, backing up originals first.
func (m *Manager) Apply(id, baseURL, apiKey, model string, models []string) (map[string]any, error) {
	c, err := m.client(id)
	if err != nil {
		return nil, err
	}
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		return nil, fmt.Errorf("base_url must start with http:// or https://")
	}
	baseV1 := base
	if !strings.HasSuffix(baseV1, "/v1") {
		baseV1 += "/v1"
	}
	ctx := applyCtx{BaseURL: base, BaseURLV1: baseV1, APIKey: apiKey, Model: strings.TrimSpace(model), Models: models}
	paths := c.paths(m.Home)
	contents, err := c.apply(m, paths, ctx)
	if err != nil {
		return nil, err
	}
	// Stable write order so a multi-file failure leaves predictable state.
	keys := make([]string, 0, len(contents))
	for k := range contents {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	state := m.loadState()
	rec := state.Clients[id]
	prior := map[string]fileEntry{}
	for _, f := range rec.Files {
		prior[f.Path] = f
	}
	type staged struct {
		path     string
		content  []byte
		existed  bool
		backup   string
		previous []byte
	}
	var stage []staged
	for _, path := range keys {
		content := contents[path]
		_, statErr := os.Stat(path)
		existed := statErr == nil
		p, hadPrior := prior[path]
		if hadPrior && !p.Existed && existed {
			// We created it earlier; while it still holds exactly what we
			// wrote it stays ours, so restore deletes it again.
			if raw, err := os.ReadFile(path); err == nil && hashBytes(raw) == p.SHA256 {
				existed = false
			}
		}
		backup := ""
		var previous []byte
		if existed {
			raw, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			previous = raw
			if hadPrior && p.Backup != "" {
				backup = p.Backup // keep the very first pre-integration backup
			} else {
				backup, err = m.backupFile(id, path, raw)
				if err != nil {
					return nil, err
				}
			}
		}
		stage = append(stage, staged{path: path, content: content, existed: existed, backup: backup, previous: previous})
	}

	var written []staged
	var entries []fileEntry
	for _, s := range stage {
		if err := atomicWrite(s.path, s.content, 0o600); err != nil {
			for _, w := range written {
				if w.existed {
					_ = atomicWrite(w.path, w.previous, 0o600)
				} else {
					_ = os.Remove(w.path)
				}
			}
			return nil, fmt.Errorf("failed to write %s: %w", c.label, err)
		}
		written = append(written, s)
		entries = append(entries, fileEntry{Path: s.path, Existed: s.existed, Backup: s.backup,
			SHA256: hashBytes(s.content), Bytes: len(s.content)})
	}
	state.Clients[id] = stateRecord{AppliedAt: m.Now().Format("2006-01-02 15:04:05"),
		BaseURL: base, Model: ctx.Model, Files: entries}
	if err := m.saveState(state); err != nil {
		return nil, err
	}
	return map[string]any{"client": id, "label": c.label, "applied_at": state.Clients[id].AppliedAt,
		"base_url": base, "model": ctx.Model, "files": entries}, nil
}

// Restore puts the pre-integration bytes back and clears the record.
func (m *Manager) Restore(id string) (map[string]any, error) {
	c, err := m.client(id)
	if err != nil {
		return nil, err
	}
	state := m.loadState()
	rec, ok := state.Clients[id]
	if !ok || len(rec.Files) == 0 {
		return nil, fmt.Errorf("%s 没有可恢复的接管记录", c.label)
	}
	var restored []map[string]any
	for _, f := range rec.Files {
		if f.Existed && f.Backup != "" {
			raw, err := os.ReadFile(f.Backup)
			if err != nil {
				return nil, fmt.Errorf("backup missing for %s: %v", f.Path, err)
			}
			if err := atomicWrite(f.Path, raw, 0o600); err != nil {
				return nil, err
			}
			restored = append(restored, map[string]any{"path": f.Path, "action": "restored"})
		} else {
			action := "deleted"
			if err := os.Remove(f.Path); err != nil {
				if os.IsNotExist(err) {
					action = "already-absent"
				} else {
					return nil, err
				}
			}
			pruneEmptyParents(f.Path, m.Home)
			restored = append(restored, map[string]any{"path": f.Path, "action": action})
		}
	}
	delete(state.Clients, id)
	if err := m.saveState(state); err != nil {
		return nil, err
	}
	return map[string]any{"client": id, "label": c.label, "files": restored}, nil
}

func (m *Manager) backupFile(id, path string, raw []byte) (string, error) {
	dir := filepath.Join(m.AccountsDir, "agent-backups", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s-%s", m.Now().Format("20060102-150405.000000000"), filepath.Base(path))
	dest := filepath.Join(dir, name)
	if err := atomicWrite(dest, raw, 0o600); err != nil {
		return "", err
	}
	pruneBackups(dir)
	return dest, nil
}

func pruneBackups(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) <= backupKeep {
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries[:len(entries)-backupKeep] {
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

func pruneEmptyParents(path, home string) {
	home = filepath.Clean(home)
	dir := filepath.Dir(path)
	for strings.HasPrefix(dir, home) && dir != home {
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

func hashBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func atomicWrite(path string, raw []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readJSON(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func writeJSON(path string, m map[string]any) ([]byte, error) {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

func deepMerge(base, patch map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range patch {
		if pm, ok := v.(map[string]any); ok {
			if bm, ok := out[k].(map[string]any); ok {
				out[k] = deepMerge(bm, pm)
				continue
			}
		}
		out[k] = v
	}
	return out
}

func applyClaude(m *Manager, paths map[string]string, ctx applyCtx) (map[string][]byte, error) {
	path := paths["settings"]
	data, err := readJSON(path)
	if err != nil {
		return nil, err
	}
	env, _ := data["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
	}
	env["ANTHROPIC_BASE_URL"] = strings.TrimSuffix(strings.TrimSuffix(ctx.BaseURL, "/"), "/v1")
	env["ANTHROPIC_AUTH_TOKEN"] = ctx.APIKey
	if ctx.Model != "" {
		for _, k := range []string{"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL"} {
			env[k] = ctx.Model
		}
	}
	data["env"] = env
	raw, err := writeJSON(path, data)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{path: raw}, nil
}

func applyCodex(m *Manager, paths map[string]string, ctx applyCtx) (map[string][]byte, error) {
	path := paths["config"]
	data := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(raw))) > 0 {
		if err := toml.Unmarshal(raw, &data); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	data["model_provider"] = ProviderID
	if ctx.Model != "" {
		data["model"] = ctx.Model
	}
	providers, _ := data["model_providers"].(map[string]any)
	if providers == nil {
		providers = map[string]any{}
	}
	providers[ProviderID] = map[string]any{
		"name": ProviderName, "base_url": ctx.BaseURLV1, "wire_api": "responses", "env_key": "OPENAI_API_KEY",
	}
	data["model_providers"] = providers
	raw, err := toml.Marshal(data)
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{path: raw}
	auth, err := readJSON(paths["auth"])
	if err != nil {
		return nil, err
	}
	auth["OPENAI_API_KEY"] = ctx.APIKey
	araw, err := writeJSON(paths["auth"], auth)
	if err != nil {
		return nil, err
	}
	out[paths["auth"]] = araw
	return out, nil
}

func applyOpenCode(m *Manager, paths map[string]string, ctx applyCtx) (map[string][]byte, error) {
	path := paths["config"]
	data, err := readJSON(path)
	if err != nil {
		return nil, err
	}
	if _, ok := data["$schema"]; !ok {
		data["$schema"] = "https://opencode.ai/config.json"
	}
	provider := map[string]any{
		"npm": "@ai-sdk/openai-compatible", "name": ProviderName,
		"options": map[string]any{"baseURL": ctx.BaseURLV1, "apiKey": ctx.APIKey},
	}
	if len(ctx.Models) > 0 {
		models := map[string]any{}
		for _, id := range ctx.Models {
			models[id] = map[string]any{}
		}
		provider["models"] = models
	}
	data = deepMerge(data, map[string]any{"provider": map[string]any{ProviderID: provider}})
	if ctx.Model != "" {
		data["model"] = ProviderID + "/" + ctx.Model
	}
	raw, err := writeJSON(path, data)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{path: raw}, nil
}

func applyDSH(m *Manager, paths map[string]string, ctx applyCtx) (map[string][]byte, error) {
	settingsPath := paths["settings"]
	var doc map[string]any
	if raw, err := os.ReadFile(settingsPath); err == nil && len(strings.TrimSpace(string(raw))) > 0 {
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("%s: %w", settingsPath, err)
		}
	}
	if doc == nil {
		doc = map[string]any{}
	}
	root, _ := doc["llm-pi-ai"].(map[string]any)
	if root == nil {
		root = map[string]any{}
	}
	providers, _ := root["providers"].(map[string]any)
	if providers == nil {
		providers = map[string]any{}
	}
	provider := map[string]any{
		"displayName": ProviderName, "apiKeyEnv": CredentialRef, "api": "openai-completions",
		"baseURL": ctx.BaseURLV1,
		"compat":  map[string]any{"thinkingFormat": "openai", "supportsReasoningEffort": true},
	}
	if len(ctx.Models) > 0 {
		var list []any
		for _, id := range ctx.Models {
			list = append(list, map[string]any{"id": id})
		}
		provider["models"] = list
	}
	providers[ProviderID] = provider
	root["providers"] = providers
	doc["llm-pi-ai"] = root
	if ctx.Model != "" {
		def, _ := doc["agent-default-model"].(map[string]any)
		if def == nil {
			def = map[string]any{}
		}
		def["provider"] = ProviderID
		def["model"] = ctx.Model
		doc["agent-default-model"] = def
	}
	sraw, err := yaml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{settingsPath: sraw}
	credPath := paths["credentials"]
	creds := map[string]any{}
	if raw, err := os.ReadFile(credPath); err == nil && len(strings.TrimSpace(string(raw))) > 0 {
		if err := yaml.Unmarshal(raw, &creds); err != nil {
			return nil, fmt.Errorf("%s: %w", credPath, err)
		}
	}
	refs, _ := creds["refs"].(map[string]any)
	if refs == nil {
		refs = map[string]any{}
	}
	refs[CredentialRef] = ctx.APIKey
	creds["refs"] = refs
	craw, err := yaml.Marshal(creds)
	if err != nil {
		return nil, err
	}
	out[credPath] = craw
	return out, nil
}

func applyCrush(m *Manager, paths map[string]string, ctx applyCtx) (map[string][]byte, error) {
	path := paths["config"]
	data, err := readJSON(path)
	if err != nil {
		return nil, err
	}
	var models []any
	for _, id := range ctx.Models {
		models = append(models, map[string]any{"id": id, "name": id})
	}
	provider := map[string]any{"name": ProviderName, "type": "openai", "base_url": ctx.BaseURLV1,
		"api_key": ctx.APIKey, "models": models}
	data = deepMerge(data, map[string]any{"providers": map[string]any{ProviderID: provider}})
	raw, err := writeJSON(path, data)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{path: raw}, nil
}

func applyZCode(m *Manager, paths map[string]string, ctx applyCtx) (map[string][]byte, error) {
	path := paths["config"]
	data, err := readJSON(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		data = map[string]any{"schemaVersion": 1, "config": map[string]any{
			"providerOrder": []any{}, "providerConfigRules": map[string]any{"providerRules": []any{}}}}
	}
	config, ok := data["config"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("zcode provider config has no config object: %s", path)
	}
	holder, ok := config["providerConfigRules"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("zcode provider config has no providerConfigRules: %s", path)
	}
	rules, ok := holder["providerRules"].([]any)
	if !ok {
		return nil, fmt.Errorf("zcode providerConfigRules has no providerRules list: %s", path)
	}
	rule := map[string]any{
		"providerId": ProviderID, "providerName": ProviderName,
		"config": map[string]any{
			"group":  "standard-personal",
			"access": map[string]any{"type": "api-key", "apiKey": ctx.APIKey},
			"api":    map[string]any{"type": "openai-chat-completions", "baseUrl": ctx.BaseURLV1},
		},
	}
	if len(ctx.Models) > 0 {
		ids := make([]any, 0, len(ctx.Models))
		for _, id := range ctx.Models {
			ids = append(ids, id)
		}
		inner := rule["config"].(map[string]any)
		inner["personalModelIds"] = ids
		inner["modelOrder"] = ids
	}
	replaced := false
	for i, existing := range rules {
		if em, ok := existing.(map[string]any); ok && em["providerId"] == ProviderID {
			rules[i] = deepMerge(em, rule)
			replaced = true
			break
		}
	}
	if !replaced {
		rules = append(rules, rule)
	}
	holder["providerRules"] = rules
	config["providerConfigRules"] = holder
	order, _ := config["providerOrder"].([]any)
	found := false
	for _, o := range order {
		if o == ProviderID {
			found = true
		}
	}
	if !found {
		order = append(order, ProviderID)
	}
	config["providerOrder"] = order
	data["config"] = config
	raw, err := writeJSON(path, data)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{path: raw}, nil
}
