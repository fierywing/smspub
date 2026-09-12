// Package push 实现多通道消息推送（纯网络出站）。
// 11 种通道的 type 字符串与 config 键严格对应 docs/BACKEND.md 的对照表。
package push

import (
	"net/http"
	"sync"
	"time"

	"smspub/internal/logger"
	"smspub/internal/model"
)

// defaultDelays 失败重试间隔：共 3 次尝试（首次 + 2 次重试），间隔 2s、4s。
var defaultDelays = []time.Duration{2 * time.Second, 4 * time.Second}

// Manager 持有当前推送通道集合，向所有 enabled 通道非阻塞广播。
type Manager struct {
	mu     sync.Mutex
	chans  []model.Channel
	client *http.Client    // 全部 HTTP 通道共用，超时 10s
	delays []time.Duration // 重试间隔；测试注入短值加速

	lg *logger.Logger // 可为 nil（静默）
}

// New 创建推送管理器；chans 做防御性拷贝，调用方后续修改不影响内部状态。
func New(chans []model.Channel, lg *logger.Logger) *Manager {
	return &Manager{
		chans:  copyChans(chans),
		client: &http.Client{Timeout: 10 * time.Second},
		delays: defaultDelays,
		lg:     lg,
	}
}

// Replace 热替换通道配置（配置保存后调用）；在途广播使用旧快照，互不干扰。
func (m *Manager) Replace(chans []model.Channel) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.chans = copyChans(chans)
}

// Broadcast 向所有 enabled 通道异步推送并立即返回：每个通道一个 goroutine，
// 失败按策略重试，所有错误只记日志，绝不 panic、绝不阻塞调用方。
func (m *Manager) Broadcast(title, body string) {
	for _, ch := range m.snapshot() {
		if !ch.Enabled {
			continue
		}
		go m.deliver(ch, title, body)
	}
}

// snapshot 返回通道列表的深拷贝。
func (m *Manager) snapshot() []model.Channel {
	m.mu.Lock()
	defer m.mu.Unlock()
	return copyChans(m.chans)
}

// copyChans 深拷贝通道列表（含 Config map，防止调用方后续原地修改）。
func copyChans(chans []model.Channel) []model.Channel {
	out := make([]model.Channel, len(chans))
	for i, ch := range chans {
		out[i] = ch
		out[i].Config = make(map[string]string, len(ch.Config))
		for k, v := range ch.Config {
			out[i].Config[k] = v
		}
	}
	return out
}

// deliver 单通道投递：构建发送器 → 带重试执行；任何失败都记 Errorf。
func (m *Manager) deliver(ch model.Channel, title, body string) {
	defer func() { // 双保险：推送侧任何 panic 都不允许影响主流程
		if r := recover(); r != nil {
			m.errorf("通道 %s 推送异常退出: %v", chName(ch), r)
		}
	}()
	sf, err := buildSender(ch, m.client)
	if err != nil {
		m.errorf("通道 %s 配置无效，跳过: %v", chName(ch), err)
		return
	}
	m.sendWithRetry(sf, ch, title, body)
}

// sendWithRetry 共 3 次尝试，间隔 m.delays；4xx（除 429）与配置错误不重试，
// 传输错误、5xx、429 重试。全部失败后记 Errorf。
func (m *Manager) sendWithRetry(sf sendFunc, ch model.Channel, title, body string) {
	var err error
	for attempt := 0; ; attempt++ {
		err = sf(title, body)
		if err == nil {
			return
		}
		if attempt >= len(m.delays) || !retryable(err) {
			break
		}
		time.Sleep(m.delays[attempt])
	}
	m.errorf("推送失败: 通道 %s: %v", chName(ch), err)
}

// chName 通道展示名：优先用户命名，否则用类型。
func chName(ch model.Channel) string {
	if ch.Name != "" {
		return ch.Name
	}
	return ch.Type
}

// errorf 以 scope="push" 记录错误日志；lg 为 nil 时静默。
func (m *Manager) errorf(format string, args ...any) {
	if m.lg != nil {
		m.lg.Errorf("push", format, args...)
	}
}
