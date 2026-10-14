package hub

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestVanishTrackerAlertsOncePerEpisode(t *testing.T) {
	tr := newVanishTracker(5 * time.Minute)
	clock := time.Unix(0, 0)
	tr.now = func() time.Time { return clock }

	// 首次观测：开始计时，不告警
	if got := tr.observe([]string{"a"}); len(got) != 0 {
		t.Fatalf("首次观测不应告警，得到 %v", got)
	}
	clock = clock.Add(2 * time.Minute)
	if got := tr.observe([]string{"a"}); len(got) != 0 {
		t.Fatalf("未跨过 grace 不应告警，得到 %v", got)
	}
	clock = clock.Add(4 * time.Minute) // 累计 6 分钟
	if got := tr.observe([]string{"a"}); len(got) != 1 || got[0] != "a" {
		t.Fatalf("跨过 grace 应告警一次，得到 %v", got)
	}
	clock = clock.Add(30 * time.Second)
	if got := tr.observe([]string{"a"}); len(got) != 0 {
		t.Fatalf("同一 episode 不应重复告警，得到 %v", got)
	}

	// 恢复：状态清除，再次消失重新计时
	clock = clock.Add(30 * time.Second)
	if got := tr.observe(nil); len(got) != 0 {
		t.Fatalf("恢复轮不应告警，得到 %v", got)
	}
	if len(tr.since) != 0 {
		t.Fatalf("恢复后应清除状态，得到 %v", tr.since)
	}
	clock = clock.Add(2 * time.Minute)
	if got := tr.observe([]string{"a"}); len(got) != 0 {
		t.Fatalf("再次消失应重新计时，得到 %v", got)
	}
	clock = clock.Add(6 * time.Minute)
	if got := tr.observe([]string{"a"}); len(got) != 1 {
		t.Fatalf("再次跨过 grace 应重新告警，得到 %v", got)
	}
}

func TestVanishTrackerMultipleCardsIndependent(t *testing.T) {
	tr := newVanishTracker(5 * time.Minute)
	clock := time.Unix(0, 0)
	tr.now = func() time.Time { return clock }

	clock = clock.Add(1 * time.Minute)
	if got := tr.observe([]string{"a", "b"}); len(got) != 0 {
		t.Fatalf("首次观测不应告警，得到 %v", got)
	}
	// a 恢复又消失，b 持续消失
	clock = clock.Add(2 * time.Minute)
	tr.observe([]string{"a", "b"})
	clock = clock.Add(1 * time.Minute)
	tr.observe([]string{"b"}) // a 恢复
	clock = clock.Add(3 * time.Minute)
	got := tr.observe([]string{"a", "b"}) // a 再次消失；b 累计 7 分钟应告警
	if len(got) != 1 || got[0] != "b" {
		t.Fatalf("应只告警持续消失的 b，得到 %v", got)
	}
}

func TestDevicePresent(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "ttyUSB9")
	if err := os.WriteFile(existing, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !devicePresent(existing) {
		t.Fatal("存在的路径应判定为在位")
	}
	if devicePresent(filepath.Join(dir, "gone")) {
		t.Fatal("不存在的路径应判定为消失")
	}
}
