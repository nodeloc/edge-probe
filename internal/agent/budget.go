package agent

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// Budget 按自然月累计估算流量，持久化到配置旁边，重启后继续累计
type Budget struct {
	mu    sync.Mutex
	path  string
	limit int64
	state budgetState
	dirty bool
	now   func() time.Time
}

type budgetState struct {
	Month string `json:"month"`
	Bytes int64  `json:"bytes"`
}

func NewBudget(path string, limitMB int64) *Budget {
	var b = &Budget{path: path, limit: limitMB << 20, now: time.Now}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &b.state)
	}
	return b
}

func (b *Budget) roll() {
	var month = b.now().UTC().Format("200601")
	if b.state.Month != month {
		b.state = budgetState{Month: month}
		b.dirty = true
	}
}

// Allow 预留 n 字节；超出本月上限时返回 false
func (b *Budget) Allow(n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.roll()
	if b.state.Bytes+n > b.limit {
		return false
	}
	b.state.Bytes += n
	b.dirty = true
	return true
}

func (b *Budget) Used() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.roll()
	return b.state.Bytes
}

// Save 定期调用；写失败只影响重启后的累计值
func (b *Budget) Save() {
	b.mu.Lock()
	if !b.dirty {
		b.mu.Unlock()
		return
	}
	data, _ := json.Marshal(b.state)
	b.dirty = false
	b.mu.Unlock()
	var tmp = b.path + ".tmp"
	if os.WriteFile(tmp, data, 0600) == nil {
		_ = os.Rename(tmp, b.path)
	}
}
