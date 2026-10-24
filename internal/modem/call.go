package modem

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"smspub/internal/at"
	"smspub/internal/model"
)

var (
	ErrCallUnsupported = errors.New("当前固件未提供语音通话控制，请确认模块子型号和语音固件")
	ErrCallConflict    = errors.New("当前通话状态不允许此操作")
	ErrCallInput       = errors.New("通话参数无效：号码须为 1–20 位数字，可带前导 +；操作为 dial/answer/hangup")
)

func ValidDialNumber(number string) bool {
	digits := strings.TrimPrefix(number, "+")
	if len(digits) == 0 || len(digits) > 20 {
		return false
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// parseCalls 保留所有类型的呼叫。损坏的 CLCC 行返回错误，避免把未知状态当作空闲。
// 实测 ML307A 在呼叫状态变化时会主动上报 +CLCC:，若恰好插在 CLCC 查询在途期间，
// 该行会被并入命令响应（at 层把非结果行归入中间行），同一呼叫出现两次；按
// 27.007 呼叫 idx 的唯一性去重，否则重复的来电条目会卡死 answer 的单呼校验。
func parseCalls(lines []string) ([]model.Call, error) {
	calls := make([]model.Call, 0)
	seen := make(map[int]bool)
	states := []string{"active", "held", "dialing", "alerting", "incoming", "waiting"}
	for _, line := range lines {
		if !strings.HasPrefix(line, "+CLCC:") {
			continue
		}
		r := csv.NewReader(strings.NewReader(strings.TrimSpace(strings.TrimPrefix(line, "+CLCC:"))))
		r.TrimLeadingSpace = true
		fields, err := r.Read()
		if err != nil || len(fields) < 5 {
			return nil, fmt.Errorf("无法解析通话状态: %s", line)
		}
		v := make([]int, 5)
		for i := range v {
			v[i], err = strconv.Atoi(strings.TrimSpace(fields[i]))
			if err != nil {
				return nil, fmt.Errorf("无法解析通话状态: %s", line)
			}
		}
		if v[0] < 1 || v[1] < 0 || v[1] > 1 || v[2] < 0 || v[2] >= len(states) || v[3] < 0 || v[3] > 9 {
			return nil, fmt.Errorf("未知通话状态: %s", line)
		}
		call := model.Call{ID: v[0], Direction: "out", State: states[v[2]], Mode: v[3]}
		if v[1] == 1 {
			call.Direction = "in"
		}
		if len(fields) > 5 {
			call.Number = fields[5]
		}
		if len(fields) > 6 && strings.TrimSpace(fields[6]) == "145" && call.Number != "" && !strings.HasPrefix(call.Number, "+") {
			call.Number = "+" + call.Number
		}
		if seen[call.ID] {
			continue
		}
		seen[call.ID] = true
		calls = append(calls, call)
	}
	return calls, nil
}

func (m *Modem) VoiceStatus() model.VoiceStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := m.voice
	v.Calls = append([]model.Call{}, v.Calls...)
	return v
}

func (m *Modem) initVoice(ctx context.Context, cli *at.Client) {
	m.callMu.Lock()
	defer m.callMu.Unlock()
	// CLCC 本身可能存在于数据版固件，已知无语音子型号不能据此宣称支持。
	hw := strings.ToUpper(m.currentModel())
	if strings.Contains(hw, "ML307A-DCLN") || strings.Contains(hw, "ML307A-GCLN") || strings.Contains(hw, "ML307A-DL") {
		return
	}
	if err := m.refreshCalls(ctx, cli); err != nil {
		m.logf("info", "语音通话查询不可用: %v", err)
		return
	}
	if _, err := m.cmd(ctx, cli, "AT+CLIP=1", m.initTimeout); err != nil {
		m.logf("warn", "来电显示上报开启失败，继续使用 CLCC 查询: %v", err)
	}
}

// 调用方持有 callMu。连接身份校验防止旧连接的结果污染重连后的状态。
func (m *Modem) refreshCalls(ctx context.Context, cli *at.Client) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	lines, err := m.cmd(ctx, cli, "AT+CLCC", m.initTimeout)
	var calls []model.Call
	if err == nil {
		calls, err = parseCalls(lines)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.curConn == nil || m.curConn.cli != cli {
		return at.ErrClosed
	}
	if err != nil {
		m.voice.Error = "通话状态查询失败: " + err.Error()
		return err
	}
	m.voice = model.VoiceStatus{Supported: true, Calls: calls}
	return nil
}

func callEvent(line string) bool {
	return line == "RING" || line == "NO CARRIER" || line == "BUSY" || line == "NO ANSWER" ||
		strings.HasPrefix(line, "+CRING:") || strings.HasPrefix(line, "+CLIP:") ||
		strings.HasPrefix(line, "+CLCC:") ||
		strings.HasPrefix(line, "+CIEV:") || strings.HasPrefix(line, "CIEV:") || strings.HasPrefix(line, "NO D")
}

func (m *Modem) wakeCalls(cli *at.Client) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.curConn; c != nil && c.cli == cli {
		select {
		case c.callWake <- struct{}{}:
		default:
		}
	}
}

func (m *Modem) callLoop(ctx context.Context, c *conn) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		case <-t.C:
		case <-c.callWake:
		}
		if !c.cli.Alive() || !m.VoiceStatus().Supported {
			continue
		}
		m.callMu.Lock()
		_ = m.refreshCalls(ctx, c.cli)
		m.callMu.Unlock()
	}
}

// ControlCall 实现单通语音的拨打、接听和全部挂断；从不重试拨号/接听。
// 返回 OK 只表示命令被接受，是否接通以随后 CLCC 查询为准。
func (m *Modem) ControlCall(ctx context.Context, action, number string) (model.VoiceStatus, error) {
	if (action != "dial" && action != "answer" && action != "hangup") || (action == "dial" && !ValidDialNumber(number)) {
		return m.VoiceStatus(), ErrCallInput
	}
	m.callMu.Lock()
	defer m.callMu.Unlock()
	cli, err := m.client()
	if err != nil {
		return m.VoiceStatus(), err
	}
	if !m.VoiceStatus().Supported {
		return m.VoiceStatus(), ErrCallUnsupported
	}
	if err := ctx.Err(); err != nil {
		return m.VoiceStatus(), err
	}
	// 挂断即使查询失败也可执行，用户始终可以尝试停止呼叫。
	if err := m.refreshCalls(ctx, cli); err != nil && action != "hangup" {
		return m.VoiceStatus(), err
	}
	calls := m.VoiceStatus().Calls
	cmd := "ATH"
	switch action {
	case "dial":
		if len(calls) != 0 {
			return m.VoiceStatus(), ErrCallConflict
		}
		cmd = "ATD" + number + ";"
	case "answer":
		if len(calls) != 1 || calls[0].State != "incoming" || !calls[0].IsVoice() {
			return m.VoiceStatus(), ErrCallConflict
		}
		cmd = "ATA"
	}
	_, err = m.cmd(ctx, cli, cmd, 30*time.Second)
	if err != nil {
		// ATH 释放呼叫时固件以 NO CARRIER 等呼叫结果行结束命令（实测 ML307A
		// 行为，亦是 Hayes 传统语义），呼叫确已终止；ATA 瞬间对端挂断同理。
		// 命令被接受与否以随后的 CLCC 复查为准，不把这类结果报成失败。
		var callErr *at.Error
		if (action == "hangup" || action == "answer") && errors.As(err, &callErr) && callErr.Kind == "CALL" {
			err = nil
		}
	}
	if err != nil {
		return m.VoiceStatus(), fmt.Errorf("通话操作失败: %w", err)
	}
	// 状态查询失败不会把已执行的操作报告为失败，避免用户重复拨号。
	_ = m.refreshCalls(ctx, cli)
	return m.VoiceStatus(), nil
}
