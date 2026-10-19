package shape

import (
	"sync"
	"time"
)

// DefaultGatewayPrompt is the built-in prompt of the custom/append modes.
const DefaultGatewayPrompt = `你是一名工程助手，幫助用戶完成軟件工程任務。

- 先理解代碼與上下文再動手，遵循既有模式與約定。
- 最小改動：只改必要部分，不做無關重構。
- 改動後要驗證：跑測試或構建確認結果，不假設「應該沒問題」。
- 對不確定的事保持誠實：說明不確定並給出驗證路徑，不編造。
- 跟隨用戶語言；簡潔直接，技術術語精確。
- 如實報告失敗與邊界，不掩蓋、不粉飾。
`

// DegradeGate is the process-memory window in which passthrough/append prompt
// modes fall back to the minimal neutral prompt. It starts when the upstream
// rejects content (403) and ends at the next 00:00 CST. Restarts clear it.
type DegradeGate struct {
	mu    sync.Mutex
	until time.Time
}

// NextMidnightCST returns the next Asia/Shanghai 00:00 after now (fixed +08:00,
// independent of the host time zone).
func NextMidnightCST(now time.Time) time.Time {
	const offset = 8 * 3600
	day := (now.Unix() + offset) / 86400
	return time.Unix((day+1)*86400-offset, 0)
}

// Active reports whether the degrade window is open.
func (g *DegradeGate) Active(now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return now.Before(g.until)
}

// Trigger opens the window until the next 00:00 CST (never extends it).
func (g *DegradeGate) Trigger(now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !now.Before(g.until) {
		g.until = NextMidnightCST(now)
	}
}

// Reset closes the window.
func (g *DegradeGate) Reset() {
	g.mu.Lock()
	g.until = time.Time{}
	g.mu.Unlock()
}
