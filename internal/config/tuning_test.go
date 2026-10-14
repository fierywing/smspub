package config

// tuning 段测试：round-trip、深拷贝隔离、负值归一化。

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"smspub/internal/logger"
	"smspub/internal/model"
)

func TestTuningRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s, err := Load(path, logger.New(16))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg := s.Get()
	cfg.Tuning = &model.TuningConfig{
		HeartbeatIntervalSec:  60,
		HeartbeatFailures:     2,
		PollIntervalSec:       300,
		ReconnectBaseMs:       1000,
		ReconnectMaxMs:        30000,
		JitterMs:              5000,
		DetectIntervalSec:     120,
		ReadTimeoutMs:         400,
		USBResetAfterFailures: 5,
		USBResetThrottleSec:   90,
	}
	if err := s.Set(cfg); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// 磁盘 JSON 含 tuning 段
	data, err := json.Marshal(s.Get())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["tuning"]; !ok {
		t.Fatal("落盘 JSON 缺少 tuning 段")
	}

	// 重新 Load 后字段保留
	s2, err := Load(path, logger.New(16))
	if err != nil {
		t.Fatalf("重新 Load: %v", err)
	}
	got := s2.Get().Tuning
	if got == nil || got.HeartbeatIntervalSec != 60 || got.PollIntervalSec != 300 ||
		got.ReconnectMaxMs != 30000 || got.DetectIntervalSec != 120 ||
		got.USBResetAfterFailures != 5 || got.USBResetThrottleSec != 90 {
		t.Fatalf("tuning round-trip 失败: %+v", got)
	}
}

func TestTuningCloneIsolation(t *testing.T) {
	tu := &model.TuningConfig{PollIntervalSec: 42}
	s := &Store{cfg: model.Config{Tuning: tu}}
	got := s.Get()
	got.Tuning.PollIntervalSec = 99
	if tu.PollIntervalSec != 42 {
		t.Fatal("Get 未深拷贝 Tuning，修改穿透到内部状态")
	}
}

func TestTuningNegativeNormalized(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s, err := Load(path, logger.New(16))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg := s.Get()
	cfg.Tuning = &model.TuningConfig{PollIntervalSec: -5, HeartbeatIntervalSec: -1, USBResetThrottleSec: -7}
	if err := s.Set(cfg); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := s.Get().Tuning; got.PollIntervalSec != 0 || got.HeartbeatIntervalSec != 0 || got.USBResetThrottleSec != 0 {
		t.Fatalf("负值应归一化为 0: %+v", got)
	}
	// 0 = 契约默认
	r := s.Get().Tuning.Resolved()
	if r.PollIntervalSec != 20 || r.HeartbeatIntervalSec != 30 ||
		r.USBResetAfterFailures != 3 || r.USBResetThrottleSec != 180 {
		t.Fatalf("Resolved 默认值错误: %+v", r)
	}
}
