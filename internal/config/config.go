// Package config 负责 config.json 的加载、校验与原子落盘（写临时文件后 rename）。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"smspub/internal/logger"
	"smspub/internal/model"
)

// Store 持有当前配置：Get/Set 并发安全，落盘失败时不改动内存中的配置。
type Store struct {
	mu   sync.RWMutex
	path string
	cfg  model.Config
	lg   *logger.Logger
}

// Load 从 path 读取配置；文件不存在时创建默认配置。
func Load(path string, lg *logger.Logger) (*Store, error) {
	if path == "" {
		return nil, errors.New("config: 配置文件路径不能为空")
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("config: 创建配置目录失败: %w", err)
		}
	}
	s := &Store{path: path, lg: lg}

	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, &s.cfg); err != nil {
			return nil, fmt.Errorf("config: 解析 %s 失败: %w", path, err)
		}
		normalizeTuning(s.cfg)
		// 旧配置文件没有 auto_cards 字段：bool 零值为 false，但该功能应对存量用户
		// 默认开启，故缺字段时归一化为 true（显式写 false 的不受影响）
		var raw map[string]json.RawMessage
		if json.Unmarshal(data, &raw) == nil {
			if _, present := raw["auto_cards"]; !present {
				s.cfg.AutoCards = true
			}
		}
		// 磁盘配置与 API 提交走同一套校验：手工编辑/旧版本产生的非法配置
		// （空 ID、重复设备等）在启动时即报错，而不是等卡片历史被 store 层静默拒收
		if err := validate(s.cfg); err != nil {
			return nil, err
		}
		return s, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("config: 读取 %s 失败: %w", path, err)
	}

	// 首次启动：生成默认配置并落盘
	s.cfg = defaultConfig()
	if err := s.persist(); err != nil {
		return nil, err
	}
	return s, nil
}

// defaultConfig 返回全新默认配置。
func defaultConfig() model.Config {
	return model.Config{
		Listen:     ":8080",
		PingTarget: "8.8.8.8",
		AutoCards:  true,
		Cards:      []model.CardConfig{},
		Channels:   []model.Channel{},
	}
}

// Get 返回当前配置的深拷贝，调用方可随意修改。
func (s *Store) Get() model.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneConfig(s.cfg)
}

// Set 校验并保存配置：校验失败或落盘失败均不改动现有配置。
func (s *Store) Set(cfg model.Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validate(cfg); err != nil {
		return err
	}
	old := s.cfg
	normalizeTuning(cfg)
	s.cfg = cloneConfig(cfg)
	if err := s.persist(); err != nil {
		s.cfg = old // 落盘失败回滚内存，保持与磁盘一致
		return err
	}
	return nil
}

// cardName 生成卡片在错误信息中的可读名称。
func cardName(c model.CardConfig, idx int) string {
	switch {
	case c.ID != "":
		return c.ID
	case c.Label != "":
		return c.Label
	default:
		return fmt.Sprintf("#%d", idx+1)
	}
}

// validCardID 只允许 [a-zA-Z0-9_-] 且非空，规则与 store.validCardID（旧版 JSONL
// 文件名白名单）对齐：两处必须一致，否则经 API 保存的"合法"卡片会在 store 层被
// 拒收、短信历史静默丢失。特意各自实现而非跨包复用，避免 store→config 的依赖方向。
func validCardID(id string) bool {
	if id == "" {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// validate 校验配置合法性，Load（磁盘读入）与 Set（API 提交）共用同一套规则：
//   - 监听地址非空；
//   - 每张卡：串口设备非空、ID 符合 validCardID 字符集且全局唯一、
//     设备路径在所有卡片间唯一（两卡共用同一串口会互相争抢）。
func validate(cfg model.Config) error {
	if strings.TrimSpace(cfg.Listen) == "" {
		return errors.New("config: 监听地址不能为空")
	}
	seenID := make(map[string]bool, len(cfg.Cards))
	seenDev := make(map[string]bool, len(cfg.Cards))
	for i := range cfg.Cards {
		c := cfg.Cards[i]
		if strings.TrimSpace(c.Device) == "" {
			return fmt.Errorf("config: 卡片 %s 的串口设备不能为空", cardName(c, i))
		}
		if !validCardID(c.ID) {
			return fmt.Errorf("config: 卡片 %s 的 ID 非法: %q（不能为空，仅允许字母、数字、下划线、连字符）",
				cardName(c, i), c.ID)
		}
		if seenID[c.ID] {
			return fmt.Errorf("config: 卡片 ID 重复: %s", c.ID)
		}
		seenID[c.ID] = true
		dev := strings.TrimSpace(c.Device)
		if seenDev[dev] {
			return fmt.Errorf("config: 卡片 %s 的串口设备 %s 与其他卡片重复", cardName(c, i), c.Device)
		}
		seenDev[dev] = true
	}
	return nil
}

// persist 把当前配置原子落盘：写同目录临时文件、fsync 后 rename 覆盖。
// 调用方需已持有写锁（Load 的首次创建阶段无并发，等效安全）。
func (s *Store) persist() error {
	data, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("config: 序列化配置失败: %w", err)
	}
	data = append(data, '\n')

	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("config: 创建临时文件失败: %w", err)
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, s.path)
	}
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("config: 保存配置失败: %w", err)
	}
	return nil
}

// cloneConfig 深拷贝配置，隔离内部状态与调用方的修改。
func cloneConfig(cfg model.Config) model.Config {
	out := cfg
	out.Cards = make([]model.CardConfig, len(cfg.Cards))
	copy(out.Cards, cfg.Cards)
	for i := range out.Cards {
		if cfg.Cards[i].Blacklist != nil {
			out.Cards[i].Blacklist = append([]string(nil), cfg.Cards[i].Blacklist...)
		}
	}
	out.Channels = make([]model.Channel, len(cfg.Channels))
	copy(out.Channels, cfg.Channels)
	for i := range out.Channels {
		if cfg.Channels[i].Config != nil {
			out.Channels[i].Config = make(map[string]string, len(cfg.Channels[i].Config))
			for k, v := range cfg.Channels[i].Config {
				out.Channels[i].Config[k] = v
			}
		}
	}
	if cfg.Tuning != nil {
		t := *cfg.Tuning
		out.Tuning = &t
	}
	return out
}

// normalizeTuning 把 tuning 段的负值归一化为 0（0 = 契约默认值）。
func normalizeTuning(cfg model.Config) {
	if cfg.Tuning == nil {
		return
	}
	t := cfg.Tuning
	for _, v := range []*int{
		&t.HeartbeatIntervalSec, &t.HeartbeatFailures, &t.PollIntervalSec,
		&t.ReconnectBaseMs, &t.ReconnectMaxMs, &t.JitterMs,
		&t.DetectIntervalSec, &t.ReadTimeoutMs,
	} {
		if *v < 0 {
			*v = 0
		}
	}
}
