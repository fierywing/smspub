// Package store 以 SQLite 持久化全部短信历史（单库 <dir>/smspub.db，驱动 modernc.org/sqlite
// 纯 Go 无 CGO，保住交叉编译能力）。默认回滚日志模式：单连接串行执行本就无读写争抢，
// 且提交后无伴生文件、库文件始终自包含，备份即拷贝；busy_timeout 兜底外部工具持锁。
// 旧版每卡一个 JSONL 文件的历史数据在 Open 时一次性自动迁移。
package store

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动（经用户批准引入的唯一新依赖）

	"smspub/internal/model"
)

// dbFile SQLite 库文件名（固定在数据目录下）。
const dbFile = "smspub.db"

// schema 建表与索引（幂等）。From/To 映射为 sender/recipient，避免 SQL 保留字 FROM。
var schema = []string{
	`CREATE TABLE IF NOT EXISTS messages(
		id         TEXT PRIMARY KEY,
		card_id    TEXT NOT NULL,
		dir        TEXT NOT NULL,
		sender     TEXT NOT NULL DEFAULT '',
		recipient  TEXT NOT NULL DEFAULT '',
		ts         INTEGER NOT NULL,
		text       TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS idx_messages_card_ts ON messages(card_id, ts DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_messages_ts ON messages(ts DESC)`,
}

// insertSQL 写入用 INSERT，主键冲突按 id 原地更新其余列（同 ID 重放覆盖旧记录）。
// 不用 INSERT OR REPLACE：它对冲突行是 DELETE+INSERT，会换 rowid，破坏
// Recent/History 以 rowid 兜底的稳定次序；ON CONFLICT DO UPDATE 保持 rowid 不变。
const insertSQL = `INSERT INTO messages(id, card_id, dir, sender, recipient, ts, text)
	VALUES(?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET card_id=excluded.card_id, dir=excluded.dir,
		sender=excluded.sender, recipient=excluded.recipient, ts=excluded.ts, text=excluded.text`

// deleteBatch DeleteMessages 每批 IN 子句的占位符个数（规避 SQLite 变量数上限，
// web 层单次上限 10000 时最多 20 批）。
const deleteBatch = 500

// Store 短信历史存储。所有方法并发安全（database/sql 连接池自身线程安全，
// 且仅 1 个连接天然串行执行）。
type Store struct {
	db *sql.DB
}

// validCardID 只允许 [a-zA-Z0-9_-]，作为 card_id 白名单校验（同旧版 JSONL 文件名规则）。
// 注意与 config 包的 validCardID 规则保持对齐（config 层在保存配置时做同一字符集
// 校验，两处若不一致会导致 store 层拒收卡片历史）；各自实现，避免跨包依赖。
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

// Open 打开（必要时创建）SQLite 历史库；目录不存在时自动创建。
// 建表后做一次性迁移：若库为空且目录残留旧版 sms-<cardID>.jsonl，逐行导入（坏行跳过），
// 原文件改名为 <原名>.imported 保留（不删除用户数据），迁移条数记入日志。
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("store: 数据目录不能为空")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("store: 创建数据目录 %s 失败: %w", dir, err)
	}
	// pragma 经 DSN 传入：驱动每建一条连接都会应用（当前仅 1 条）。
	// journal_mode 显式设为 DELETE（即默认回滚日志）：一是把曾开过 WAL 的旧库在打开时
	// 自动合并切回单文件（journal_mode 是库文件持久属性，光删 DSN 参数切不回来），
	// 二是杜绝 -wal/-shm 伴生文件，备份与现场排查都只看 smspub.db 一个文件
	dsn := "file:" + filepath.Join(dir, dbFile) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(DELETE)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: 打开 SQLite 失败: %w", err)
	}
	// 家用规模写入频度极低：单连接串行化全部 SQL，杜绝并发写锁冲突（SQLITE_BUSY），
	// 代价仅为读写互斥——对每秒几条的短信流量毫无影响
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.migrateJSONL(dir); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// init 建表与索引（语句幂等，重复 Open 安全）。
func (s *Store) init() error {
	for _, stmt := range schema {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("store: 初始化表结构失败: %w", err)
		}
	}
	return nil
}

// migrateJSONL 一次性迁移旧版 JSONL 历史：仅当 messages 表为空时执行（有数据即跳过，
// 保证幂等）。逐行解析导入（坏行跳过），完成后把原文件改名为 <原名>.imported。
// store 不依赖 logger 包，沿用标准 log 输出到 stderr。
func (s *Store) migrateJSONL(dir string) error {
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count); err != nil {
		return fmt.Errorf("store: 统计历史消息数失败: %w", err)
	}
	if count > 0 {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("store: 读取数据目录失败: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "sms-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		cardID := strings.TrimSuffix(strings.TrimPrefix(name, "sms-"), ".jsonl")
		if !validCardID(cardID) {
			log.Printf("[store] 跳过卡片 ID 非法的历史文件 %s", name)
			continue
		}
		n, err := s.importJSONL(filepath.Join(dir, name), cardID)
		if err != nil {
			return err
		}
		if err := os.Rename(filepath.Join(dir, name), filepath.Join(dir, name+".imported")); err != nil {
			// 改名失败不致命：表已非空，下次 Open 不会重复迁移
			log.Printf("[store] 警告：迁移后改名 %s 失败: %v", name, err)
		}
		log.Printf("[store] 已从 %s 迁移 %d 条历史到 SQLite", name, n)
	}
	return nil
}

// importJSONL 逐行解析一个 JSONL 历史文件并入库，返回导入条数。整个文件的导入
// 包在单个事务里：任一行写库失败或读取中断即整体回滚并报错（错误保留文件名与
// 行号）——否则半截导入后表已非空，下次 Open 会跳过迁移，剩余行永不导入。
// 坏行（含空行、半截 JSON、非 JSON、空 ID）仍按旧规则跳过。
func (s *Store) importJSONL(path, cardID string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("store: 打开历史文件 %s 失败: %w", path, err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20) // 单行上限 1MiB，容纳合并后的长短信
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("store: 开启导入事务失败: %w", err)
	}
	defer tx.Rollback() // 失败路径回滚；提交成功后 Rollback 返回 ErrTxDone，可忽略
	n, line := 0, 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var msg model.Message
		if json.Unmarshal([]byte(text), &msg) != nil || msg.ID == "" {
			continue
		}
		if _, err := tx.Exec(insertSQL, msg.ID, cardID, string(msg.Dir),
			msg.From, msg.To, msg.Ts, msg.Text); err != nil {
			return n, fmt.Errorf("store: 导入 %s 第 %d 行失败: %w", path, line, err)
		}
		n++
	}
	if err := sc.Err(); err != nil {
		return n, fmt.Errorf("store: 读取 %s 在第 %d 行附近中断: %w", path, line, err)
	}
	if err := tx.Commit(); err != nil {
		return n, fmt.Errorf("store: 提交 %s 的导入事务失败: %w", path, err)
	}
	return n, nil
}

// Append 写入一条短信记录。cardID 同样用 validCardID 白名单校验。
func (s *Store) Append(cardID string, msg model.Message) error {
	if !validCardID(cardID) {
		return fmt.Errorf("store: 非法的卡片 ID %q（仅允许字母、数字、下划线、连字符）", cardID)
	}
	if _, err := s.db.Exec(insertSQL, msg.ID, cardID, string(msg.Dir),
		msg.From, msg.To, msg.Ts, msg.Text); err != nil {
		return fmt.Errorf("store: 写入历史失败: %w", err)
	}
	return nil
}

// Recent 返回某卡最近 n 条短信（ts DESC, rowid DESC），最新在前；不足 n 条返回全部，
// n<=0、非法 cardID 或无记录返回空切片。rowid 作同毫秒消息的稳定次序兜底。
func (s *Store) Recent(cardID string, n int) []model.Message {
	out := []model.Message{}
	if n <= 0 || !validCardID(cardID) {
		return out
	}
	rows, err := s.db.Query(`SELECT id, dir, sender, recipient, ts, text FROM messages
		WHERE card_id = ? ORDER BY ts DESC, rowid DESC LIMIT ?`, cardID, n)
	if err != nil {
		log.Printf("[store] 查询卡片 %s 最近消息失败: %v", cardID, err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var m model.Message
		var dir string
		if err := rows.Scan(&m.ID, &dir, &m.From, &m.To, &m.Ts, &m.Text); err != nil {
			return out
		}
		m.Dir = model.Direction(dir)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		log.Printf("[store] 遍历卡片 %s 最近消息失败: %v", cardID, err)
	}
	return out
}

// History 返回全库全部消息（所有卡，ts DESC, rowid DESC，最新在前），携带所属卡片 ID。
func (s *Store) History() []model.HistMessage {
	out := []model.HistMessage{}
	rows, err := s.db.Query(`SELECT id, card_id, dir, sender, recipient, ts, text FROM messages
		ORDER BY ts DESC, rowid DESC`)
	if err != nil {
		log.Printf("[store] 查询历史消息失败: %v", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var h model.HistMessage
		var dir string
		if err := rows.Scan(&h.ID, &h.CardID, &dir, &h.From, &h.To, &h.Ts, &h.Text); err != nil {
			return out
		}
		h.Dir = model.Direction(dir)
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		log.Printf("[store] 遍历历史消息失败: %v", err)
	}
	return out
}

// DeleteMessages 按 ID 批量删除消息，返回实际删除的条数；空切片直接返回 0, nil。
// 分批 IN 删除（规避变量数上限）；批次间不包事务，最坏情况是中途失败时部分已删。
func (s *Store) DeleteMessages(ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var total int64
	for start := 0; start < len(ids); start += deleteBatch {
		end := min(start+deleteBatch, len(ids))
		part := ids[start:end]
		ph := make([]string, len(part))
		args := make([]any, len(part))
		for i, id := range part {
			ph[i] = "?"
			args[i] = id
		}
		res, err := s.db.Exec("DELETE FROM messages WHERE id IN ("+strings.Join(ph, ",")+")", args...)
		if err != nil {
			return total, fmt.Errorf("store: 删除历史消息失败: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("store: 获取删除条数失败: %w", err)
		}
		total += n
	}
	return total, nil
}

// Close 关闭底层 SQLite 连接。
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}
