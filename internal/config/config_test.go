package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"smspub/internal/logger"
	"smspub/internal/model"
)

func sampleConfig() model.Config {
	return model.Config{
		Listen:     ":9090",
		PingTarget: "1.1.1.1",
		Cards: []model.CardConfig{
			{ID: "c1", Device: "/dev/ttyUSB0", Baud: 115200, Number: "+8613800138000",
				Label: "卡1", Admin: "+8613900139000", Blacklist: []string{"10086", "+8610000"}},
			{ID: "c2", Device: "/dev/ttyUSB1"},
		},
		Channels: []model.Channel{
			{ID: "ch1", Type: "Bark", Name: "bark", Enabled: true, Config: map[string]string{"url": "https://api.day.app/xxx"}},
		},
	}
}

func TestLoadDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	lg := logger.New(16)
	s, err := Load(path, lg)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	cfg := s.Get()
	if cfg.Listen != ":8080" || cfg.PingTarget != "8.8.8.8" {
		t.Fatalf("默认值错误: %+v", cfg)
	}
	if !cfg.AutoCards {
		t.Fatalf("默认配置应开启 auto_cards: %+v", cfg)
	}
	if cfg.Cards == nil || cfg.Channels == nil {
		t.Fatalf("默认配置的 Cards/Channels 应为空切片而非 nil: %+v", cfg)
	}
	// 首次创建即落盘且内容合法
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("默认配置未写入磁盘: %v", err)
	}
	var onDisk model.Config
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatalf("磁盘配置不是合法 JSON: %v", err)
	}
	// 再次 Load：配置保持不变
	s2, err := Load(path, lg)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.Get(); !reflect.DeepEqual(got, cfg) {
		t.Fatalf("重复 Load 不应改变配置:\n got %+v\nwant %+v", got, cfg)
	}
}

func TestRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s, err := Load(path, logger.New(16))
	if err != nil {
		t.Fatal(err)
	}
	want := sampleConfig()
	if err := s.Set(want); err != nil {
		t.Fatalf("Set 失败: %v", err)
	}
	if got := s.Get(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Set 后 Get 应与写入值一致:\n got %+v\nwant %+v", got, want)
	}
	// Set 存的是拷贝：修改调用方副本不影响内部状态
	want.Listen = ":7777"
	if got := s.Get().Listen; got != ":9090" {
		t.Fatalf("修改调用方副本不应影响内部配置，得到 %q", got)
	}
	// 从磁盘重新加载后一致
	s2, err := Load(path, logger.New(16))
	if err != nil {
		t.Fatal(err)
	}
	want.Listen = ":9090"
	if got := s2.Get(); !reflect.DeepEqual(got, want) {
		t.Fatalf("重新加载后应与保存值一致:\n got %+v\nwant %+v", got, want)
	}
}

func TestAutoCardsDefaults(t *testing.T) {
	// 全新默认配置：自动检测开启
	s, err := Load(filepath.Join(t.TempDir(), "config.json"), logger.New(16))
	if err != nil {
		t.Fatal(err)
	}
	if !s.Get().AutoCards {
		t.Fatal("默认配置应开启 auto_cards")
	}

	// 旧版配置文件（无 auto_cards 字段）：视为开启
	old := `{"listen":":8080","ping_target":"8.8.8.8","cards":[],"channels":[]}`
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	s2, err := Load(path, logger.New(16))
	if err != nil {
		t.Fatal(err)
	}
	if !s2.Get().AutoCards {
		t.Fatal("缺 auto_cards 字段的旧配置应视为开启")
	}

	// 显式关闭：保留 false
	off := `{"listen":":8080","ping_target":"8.8.8.8","auto_cards":false,"cards":[],"channels":[]}`
	path2 := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path2, []byte(off), 0o600); err != nil {
		t.Fatal(err)
	}
	s3, err := Load(path2, logger.New(16))
	if err != nil {
		t.Fatal(err)
	}
	if s3.Get().AutoCards {
		t.Fatal("显式 auto_cards=false 不应被覆盖")
	}
}

func TestAtomicWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s, err := Load(path, logger.New(16))
	if err != nil {
		t.Fatal(err)
	}
	cfg := s.Get()
	cfg.Listen = ":9092"
	if err := s.Set(cfg); err != nil {
		t.Fatal(err)
	}
	// 临时文件已消失（rename 成功）
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("rename 后临时文件应不存在: %v", err)
	}
	// 磁盘内容与内存一致
	s2, err := Load(path, logger.New(16))
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.Get().Listen; got != ":9092" {
		t.Fatalf("磁盘配置未更新: %q", got)
	}

	// 落盘失败路径：目录只读 → Set 报错，内存与磁盘均保持旧值
	before := s.Get()
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	cfg = s.Get()
	cfg.Listen = ":9999"
	if err := s.Set(cfg); err == nil {
		t.Fatal("目录只读时 Set 应返回错误")
	}
	if got := s.Get(); !reflect.DeepEqual(got, before) {
		t.Fatalf("落盘失败不应改动内存配置:\n got %+v\nwant %+v", got, before)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("失败的 Set 不应残留临时文件: %v", err)
	}
}

func TestValidationFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s, err := Load(path, logger.New(16))
	if err != nil {
		t.Fatal(err)
	}
	// 先写入一份合法配置作为基线
	if err := s.Set(sampleConfig()); err != nil {
		t.Fatalf("基线 Set 失败: %v", err)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mutate func(*model.Config)
	}{
		{"listen 为空", func(c *model.Config) { c.Listen = "" }},
		{"listen 全空白", func(c *model.Config) { c.Listen = "   " }},
		{"卡片设备为空", func(c *model.Config) { c.Cards = []model.CardConfig{{ID: "c1", Device: "  "}} }},
		{"卡片 ID 重复", func(c *model.Config) {
			c.Cards = []model.CardConfig{{ID: "c1", Device: "/dev/a"}, {ID: "c1", Device: "/dev/b"}}
		}},
		// ID 非法会导致卡片历史被 store 层静默拒收；设备重复会让两张卡
		// 争抢同一串口
		{"卡片 ID 为空", func(c *model.Config) { c.Cards = []model.CardConfig{{ID: "", Device: "/dev/a"}} }},
		{"卡片 ID 含斜杠", func(c *model.Config) { c.Cards = []model.CardConfig{{ID: "../evil", Device: "/dev/a"}} }},
		{"卡片 ID 含中文", func(c *model.Config) { c.Cards = []model.CardConfig{{ID: "卡1", Device: "/dev/a"}} }},
		{"卡片设备重复", func(c *model.Config) {
			c.Cards = []model.CardConfig{{ID: "c1", Device: "/dev/a"}, {ID: "c2", Device: "/dev/a"}}
		}},
	}
	for _, tc := range cases {
		bad := sampleConfig()
		tc.mutate(&bad)
		if err := s.Set(bad); err == nil {
			t.Errorf("%s: Set 应返回错误", tc.name)
		}
		if got := s.Get(); got.Listen != ":9090" || len(got.Cards) != 2 {
			t.Errorf("%s: 校验失败不应改动内存配置: %+v", tc.name, got)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != string(onDisk) {
			t.Errorf("%s: 校验失败不应落盘", tc.name)
		}
	}
}

func TestLoadBrokenJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{invalid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, logger.New(16)); err == nil {
		t.Fatal("解析失败的配置应返回错误")
	}
}

func TestConcurrentGetSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s, err := Load(path, logger.New(16))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				_ = s.Get()
				cfg := s.Get()
				cfg.Listen = ":8081"
				if err := s.Set(cfg); err != nil {
					t.Errorf("并发 Set 失败: %v", err)
				}
			}
		}(g)
	}
	wg.Wait()
	if got := s.Get(); got.Listen != ":8081" {
		t.Fatalf("并发写入后配置应为最后成功的值: %+v", got)
	}
}
