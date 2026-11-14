# 系统提示词净化（对付 403 / 400 内容审核）

上游（以及一些中转）的内容审核会把**客户端身份句**当成指纹匹配，命中就整条请求拒掉：
`content rejected by upstream policy (code 11-128)` / "识别到了非法请求"。典型触发句：

- Codex CLI 的 `You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant. Codex CLI is an open source project led by OpenAI.`
- Claude Code 的 `You are Claude Code, Anthropic's official CLI tool for Claude.`
- DSH 的 `You are an AI assistant built on DeepSeek Harness.`

网关有两条路应付：**改网关侧（P2，三档强度）** 或 **改客户端（B，只动身份句）**。

---

## 一、网关侧：三档强度（面板「设置 → 上游与提示词」）

| 档位 | 行为 | 适用 |
| --- | --- | --- |
| `identity`（默认） | 只把**身份句/厂商句**换成中性句，工具契约与流程规则逐字保留 | 推荐：既不撞审核，也不改变模型行为 |
| `full` | 整段 system prompt 换成网关的中性提示词 | 应急：确定是提示词被拦、且不在乎客户端规则时 |
| `off` | 不改写，只保留原有"最小改动指纹补丁" | 上游不拦时，最大化保真 |

配置文件里对应 `prompt.sanitize`（`settings.json`）：

```json
{"prompt": {"mode": "passthrough", "file": "", "sanitize": "identity"}}
```

等价的接口写入：

```bash
curl -s -X POST localhost:8788/api/settings \
  -H 'Content-Type: application/json' \
  -d '{"prompt": {"sanitize": "identity"}}'
```

### identity 档实际做什么

只对 **system / developer**（以及被上游回显校验的 assistant 历史）生效，user 与 tool 内容不动：

```
You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant. Codex CLI is an open source project led by OpenAI.
        ↓
You are a coding assistant.

You are Claude Code, Anthropic's official CLI tool for Claude.     ↓     You are a CLI coding assistant.

You are an AI assistant built on DeepSeek Harness.                ↓     You are a helpful AI assistant.
DeepSeek Harness（残留品牌）                                       ↓     runtime
```

`# Tools` 之后的工具契约、工作流规则、`@` 路径段落说明全部逐字保留 —— 这正是 `full`
档做不到的部分（`full` 会让模型丢掉"何时用哪个工具"的约束）。

### 还有一层：403 自动降级

若上游回 **403**，网关会自动触发降级窗口（到次日 00:00 CST）：把 system 换成中性提示词
并重试一次。这是兜底，不需要手动配。注意它**只认 403**；如果上游用 **400 + 11-128**
拒绝，不会触发（P0 待办，未包含在本次改动里）。

---

## 二、客户端侧：只改身份句（B）

改一句就够，不需要动工具/流程段落。

### Codex CLI

`~/.codex/config.toml` 里的 `model_instructions_file`，或任何注入 system prompt 的地方：

```diff
- You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.
- Codex CLI is an open source project led by OpenAI.
+ You are a coding assistant.
```

### Claude Code

`--append-system-prompt` 里如果带身份句，改成：

```diff
- You are Claude Code, Anthropic's official CLI tool for Claude.
+ You are a CLI coding assistant.
```

内置身份句无法直接覆盖时，改用网关 `identity` 档（上表），效果等价。

### 通用自检

把 system prompt 单独拿出来，确认不再包含这些词：`Codex CLI`、`OpenAI`、
`Claude Code`、`Anthropic`、`DeepSeek Harness`、`facebook/react` 之类的
"官方模板句 + 品牌 + 仓库链接" 组合。用户消息里出现同样字样通常不触发。

---

## 三、怎么确认是哪一句

1. 先发一句 `你好`（无 system）→ 200，说明账号与模型正常。
2. 加上完整 system prompt → 被拒，说明问题在提示词指纹。
3. 二分 system prompt：删一半重发，直到定位到具体句子；通常就是开头那句身份句。
4. 若同时换了中转（不同域名），拦截方可能不是 WorkBuddy 而是中转自己的审核 ——
   那种情况网关侧改写无效，只能走 B 或换渠道。
