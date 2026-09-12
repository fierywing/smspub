package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"smspub/internal/model"
)

func mustAppend(t *testing.T, s *Store, cardID, id string, ts int64) {
	t.Helper()
	if err := s.Append(cardID, model.Message{ID: id, Dir: model.DirIn, From: "+8613800138000", Text: "内容 " + id, Ts: ts}); err != nil {
		t.Fatalf("Append(%s) 失败: %v", id, err)
	}
}

func ids(msgs []model.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.ID
	}
	return out
}

func histIDs(msgs []model.HistMessage) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.ID
	}
	return out
}

// TestOpenCreatesDB Open 自动建目录、建库文件（smspub.db），且重复 Open 幂等。
func TestOpenCreatesDB(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b", "data")
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 应自动创建目录并建库: %v", err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("目录未创建: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(dir, dbFile)); err != nil || fi.IsDir() {
		t.Fatalf("库文件 %s 未创建: %v", dbFile, err)
	}
	mustAppend(t, s, "c1", "m0", 1)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// 重复 Open 同一目录：表结构幂等，数据仍在
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("重复 Open 失败: %v", err)
	}
	defer s2.Close()
	if got := s2.Recent("c1", 10); len(got) != 1 || got[0].ID != "m0" {
		t.Fatalf("重复 Open 后数据应保留，得到 %v", ids(got))
	}
}

func TestAppendAndRecentOrder(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer s.Close()
	for i := 0; i < 5; i++ {
		mustAppend(t, s, "card1", fmt.Sprintf("m%d", i), int64(1000+i))
	}
	// 最近 3 条，最新在前
	got := s.Recent("card1", 3)
	if want := []string{"m4", "m3", "m2"}; strings.Join(ids(got), ",") != strings.Join(want, ",") {
		t.Fatalf("Recent(3) = %v， want %v", ids(got), want)
	}
	// n 超过总数：返回全部
	got = s.Recent("card1", 10)
	if want := []string{"m4", "m3", "m2", "m1", "m0"}; strings.Join(ids(got), ",") != strings.Join(want, ",") {
		t.Fatalf("Recent(10) = %v， want %v", ids(got), want)
	}
	// n<=0、无记录卡片返回空
	if got := s.Recent("card1", 0); len(got) != 0 {
		t.Fatalf("Recent(0) 应为空，得到 %v", ids(got))
	}
	if got := s.Recent("nope", 5); len(got) != 0 {
		t.Fatalf("无历史卡片 Recent 应为空，得到 %v", ids(got))
	}
	// 不同卡互不干扰
	mustAppend(t, s, "card2", "x0", 1)
	if got := s.Recent("card1", 10); len(got) != 5 {
		t.Fatalf("card2 的记录不应出现在 card1: %v", ids(got))
	}
}

// TestRecentSameTsStableOrder 同毫秒消息按写入次序稳定返回（rowid 兜底，后写在前）。
func TestRecentSameTsStableOrder(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < 3; i++ {
		mustAppend(t, s, "c1", fmt.Sprintf("m%d", i), 42) // ts 全部相同
	}
	got := s.Recent("c1", 10)
	if want := []string{"m2", "m1", "m0"}; strings.Join(ids(got), ",") != strings.Join(want, ",") {
		t.Fatalf("同 ts 应按 rowid 逆序稳定返回，得到 %v， want %v", ids(got), want)
	}
}

// TestHistory 全库消息按 ts DESC、rowid DESC 排序，且携带 cardId。
func TestHistory(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	mustAppend(t, s, "card1", "a1", 100)
	mustAppend(t, s, "card2", "b1", 300)
	mustAppend(t, s, "card1", "a2", 200)
	got := s.History()
	if want := []string{"b1", "a2", "a1"}; strings.Join(histIDs(got), ",") != strings.Join(want, ",") {
		t.Fatalf("History 排序 = %v， want %v", histIDs(got), want)
	}
	byID := map[string]model.HistMessage{}
	for _, h := range got {
		byID[h.ID] = h
	}
	if byID["b1"].CardID != "card2" || byID["a1"].CardID != "card1" {
		t.Fatalf("HistMessage.CardID 不正确: %+v", got)
	}
	if byID["a2"].Dir != model.DirIn || byID["a2"].Ts != 200 || byID["a2"].From != "+8613800138000" {
		t.Fatalf("HistMessage 字段不正确: %+v", byID["a2"])
	}
	// 空库返回空切片（非 nil）
	s2, err := Open(filepath.Join(t.TempDir(), "empty"))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if h := s2.History(); h == nil || len(h) != 0 {
		t.Fatalf("空库 History 应为空切片，得到 %#v", h)
	}
}

func TestDeleteMessages(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < 4; i++ {
		mustAppend(t, s, "c1", fmt.Sprintf("m%d", i), int64(i))
	}
	// 空切片：0, nil，且不影响数据
	if n, err := s.DeleteMessages(nil); n != 0 || err != nil {
		t.Fatalf("空切片应返回 0,nil，得到 %d,%v", n, err)
	}
	// 删除存在的与不存在的：计数只算实际删除的
	n, err := s.DeleteMessages([]string{"m0", "m2", "ghost"})
	if err != nil {
		t.Fatalf("DeleteMessages: %v", err)
	}
	if n != 2 {
		t.Fatalf("删除计数 = %d，期望 2", n)
	}
	if got := s.Recent("c1", 10); len(got) != 2 {
		t.Fatalf("删除后剩余 = %v，期望 2 条", ids(got))
	}
	// 再删不存在的：0
	if n, err := s.DeleteMessages([]string{"ghost"}); n != 0 || err != nil {
		t.Fatalf("删除不存在 id 应返回 0,nil，得到 %d,%v", n, err)
	}
	// 全部删完
	if n, err := s.DeleteMessages([]string{"m1", "m3"}); err != nil || n != 2 {
		t.Fatalf("删除剩余消息失败: %d,%v", n, err)
	}
	if got := s.History(); len(got) != 0 {
		t.Fatalf("全删后 History 应为空，得到 %v", histIDs(got))
	}
}

// TestDeleteMessagesBatched 超过单批 500 个 id 时分批删除仍完整。
func TestDeleteMessagesBatched(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const total = deleteBatch + 50
	for i := 0; i < total; i++ {
		mustAppend(t, s, "c1", fmt.Sprintf("m%04d", i), int64(i))
	}
	n, err := s.DeleteMessages([]string{"m0000", "ghost"}) // 首批内 1 条，跨批断言需要全量删除
	if err != nil || n != 1 {
		t.Fatalf("首批删除失败: %d,%v", n, err)
	}
	all := make([]string, 0, total-1)
	for i := 1; i < total; i++ {
		all = append(all, fmt.Sprintf("m%04d", i))
	}
	if n, err := s.DeleteMessages(all); err != nil || n != total-1 {
		t.Fatalf("跨批删除失败: %d,%v", n, err)
	}
	if got := s.History(); len(got) != 0 {
		t.Fatalf("跨批删除后应为空，剩余 %d 条", len(got))
	}
}

func TestInvalidCardID(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, bad := range []string{"", "..", "../x", "a/b", "a\\b", "卡", "a b", "a.b", "%41"} {
		if err := s.Append(bad, model.Message{ID: "x", Text: "t", Ts: 1}); err == nil {
			t.Errorf("Append(%q) 应拒绝非法 cardID", bad)
		}
		if got := s.Recent(bad, 5); len(got) != 0 {
			t.Errorf("Recent(%q) 对非法 cardID 应返回空", bad)
		}
	}
	// 合法边界值可用
	if err := s.Append("Az_09-", model.Message{ID: "ok", Text: "t", Ts: 1}); err != nil {
		t.Errorf("合法 cardID 不应报错: %v", err)
	}
}

// TestAppendSameIDKeepsRowid 同 ID 重放覆盖旧记录但不新增行，且 rowid 保持稳定
// （INSERT ... ON CONFLICT DO UPDATE；INSERT OR REPLACE 会换 rowid，破坏
// 同 ts 消息按写入次序兜底的稳定性）。
func TestAppendSameIDKeepsRowid(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	mustAppend(t, s, "c1", "m1", 1) // rowid 1
	mustAppend(t, s, "c1", "m2", 2) // rowid 2
	// 同 ID 重放：覆盖 m1 的内容并把 ts 改成与 m2 相同，此时次序全凭 rowid
	if err := s.Append("c1", model.Message{ID: "m1", Dir: model.DirIn, From: "+8613800138000", Text: "重放覆盖", Ts: 2}); err != nil {
		t.Fatalf("重放 Append: %v", err)
	}
	if got := s.History(); len(got) != 2 {
		t.Fatalf("重放后应仍为 2 条，得到 %d: %v", len(got), histIDs(got))
	}
	// rowid 稳定时 m1 仍是更早写入的行（rowid 1），同 ts 下排在 m2 之后
	got := s.Recent("c1", 10)
	if want := []string{"m2", "m1"}; strings.Join(ids(got), ",") != strings.Join(want, ",") {
		t.Fatalf("重放后同 ts 次序 = %v，期望 rowid 稳定的 %v", ids(got), want)
	}
	// 覆盖内容生效
	if got[1].Text != "重放覆盖" || got[1].Ts != 2 {
		t.Fatalf("重放未覆盖旧记录: %+v", got[1])
	}
}

// TestRestartContinuation 模拟进程重启：新 Store 实例接续同一库，读写均正常。
func TestRestartContinuation(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		mustAppend(t, s1, "c1", fmt.Sprintf("old%d", i), int64(i))
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got := s2.Recent("c1", 10)
	if want := []string{"old2", "old1", "old0"}; strings.Join(ids(got), ",") != strings.Join(want, ",") {
		t.Fatalf("重启后应续读旧数据，得到 %v， want %v", ids(got), want)
	}
	// 重启后继续追加，新旧都在
	mustAppend(t, s2, "c1", "new0", 100)
	got = s2.Recent("c1", 2)
	if want := []string{"new0", "old2"}; strings.Join(ids(got), ",") != strings.Join(want, ",") {
		t.Fatalf("重启后追加失败，得到 %v， want %v", ids(got), want)
	}
}

// TestJSONLMigration 旧版 JSONL 文件在库为空时被一次性导入，原文件改名为 .imported；
// 表非空后再次 Open 不重复迁移。
func TestJSONLMigration(t *testing.T) {
	dir := t.TempDir()
	// 构造旧数据：2 条有效 + 1 条坏行 + 1 条空 ID 行 + 末尾残行（无换行）
	lines := []string{
		`{"id":"old1","dir":"in","text":"测试测试哈","ts":1788469994799}`,
		`{"id":"old2","dir":"in","text":"测试2\n","ts":1788470010968}`,
		`not-json-line`,
		`{"id":"","dir":"in","text":"无ID视为坏行","ts":1}`,
		`{"id":"partial","dir":"in"`,
	}
	if err := os.WriteFile(filepath.Join(dir, "sms-card3.jsonl"), []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open（含迁移）失败: %v", err)
	}
	got := s.History()
	if want := []string{"old2", "old1"}; strings.Join(histIDs(got), ",") != strings.Join(want, ",") {
		t.Fatalf("迁移导入 = %v， want %v", histIDs(got), want)
	}
	for _, h := range got {
		if h.CardID != "card3" {
			t.Fatalf("迁移记录 CardID = %q，期望 card3", h.CardID)
		}
	}
	if got[0].Text != "测试2\n" || got[1].Ts != 1788469994799 {
		t.Fatalf("迁移字段不正确: %+v", got)
	}
	// 原文件改名为 .imported，且不存在未改名的旧文件
	if _, err := os.Stat(filepath.Join(dir, "sms-card3.jsonl.imported")); err != nil {
		t.Fatalf("原文件应改名为 .imported: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sms-card3.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("原 JSONL 文件应已改名: %v", err)
	}
	// 表非空后再次 Open：不重复迁移
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got := s2.History(); len(got) != 2 {
		t.Fatalf("二次 Open 不应重复迁移，得到 %d 条", len(got))
	}
}

// TestMigrationSkipsNonHistoryFiles 迁移只认 sms-*.jsonl，其他文件不动。
func TestMigrationSkipsNonHistoryFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"config.json", "sms-卡.jsonl", "other.jsonl", "sms-.jsonl"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(`{"id":"x","dir":"in","ts":1}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := s.History(); len(got) != 0 {
		t.Fatalf("非法/无关文件不应被导入，得到 %v", histIDs(got))
	}
	for _, name := range []string{"config.json", "other.jsonl", "sms-卡.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("无关文件 %s 不应被改名: %v", name, err)
		}
	}
}

func TestConcurrentAppend(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const gs, perG = 8, 25
	var wg sync.WaitGroup
	for g := 0; g < gs; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				if err := s.Append("cc", model.Message{ID: fmt.Sprintf("g%d-%d", g, i), Dir: model.DirIn, Text: "t", Ts: 1}); err != nil {
					t.Errorf("并发追加失败: %v", err)
				}
			}
		}(g)
	}
	wg.Wait()
	got := s.History()
	if len(got) != gs*perG {
		t.Fatalf("应读到全部 %d 条，得到 %d 条", gs*perG, len(got))
	}
	seen := map[string]bool{}
	for _, m := range got {
		if seen[m.ID] {
			t.Fatalf("记录重复: %s", m.ID)
		}
		seen[m.ID] = true
	}
}
