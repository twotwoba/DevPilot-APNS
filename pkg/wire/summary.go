package wire

import (
	"encoding/json"
	"strings"
)

// 云通道摘要白名单上限（按 Unicode 字符计，不是字节）。
const (
	MaxTitleRunes   = 120
	MaxCommandRunes = 256
	MaxReplyRunes   = 80
	MaxPromptRunes  = 200
)

// 超时分层常量。两端共享，避免各写一套。
//
// v0.3.0 起等待窗「按次审批」：daemon 在 envelope.expires_at 上携带本轮审批的
// 截止时刻，relay 据此派生 pending TTL / APNs 过期 / LA stale-date。下列常量
// 降级为旧 daemon（无 expires_at）或字段缺失时的回落值。
const (
	// HookTimeoutSeconds 是写入 Claude Code / Codex hook 配置的硬超时（回落值）。
	HookTimeoutSeconds = 300
	// ApprovalWaitSeconds 是 daemon 等待手机决策的旧默认（回落值）。
	ApprovalWaitSeconds = 240
	// PendingTTLSeconds 是 relay 保留 pending approval 的旧时长（回落值）。
	PendingTTLSeconds = 300
	// APNSApprovalExpirySeconds 是审批推送 APNs 过期窗口的旧值（回落值）。
	APNSApprovalExpirySeconds = 120
	// LiveActivityStaleSeconds 是 Live Activity 卡的 stale 时间。
	LiveActivityStaleSeconds = 600
)

// DefaultApprovalWaitSeconds 是 v0.3.0 起 daemon 等待手机决策的新默认（30 分钟）。
// 「随时都能审批」：等待窗内 LA / 通知持续可批，超时转 Missed 留在收件箱而非消失。
// 可被 daemon 配置 approval_timeout_seconds 覆盖；install 写入 hook 超时 = 本值 + 60。
const DefaultApprovalWaitSeconds = 1800

// MaxApprovalWaitSeconds 是 expires_at 的合法上限钳制值（6 小时）。
// daemon 盖章与 relay 校验共用,防止配置错误把等待窗写成天文数字。
const MaxApprovalWaitSeconds = 21600

// Truncate 按 Unicode 字符截断，超出时保留 max-1 个字符并追加省略号。
func Truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	return string(r[:max-1]) + "…"
}

// Summarize 把多行文本压成单行并截断。
func Summarize(s string, max int) string {
	return Truncate(strings.Join(strings.Fields(s), " "), max)
}

// SummarizeToolInput 从 tool_input 中提取用户可读的一行摘要。
//
// 优先取 command / file_path / path / pattern，其次 AskUserQuestion 的
// questions 数组（渲染成人话，见 summarizeAskUser），最后退化到 JSON 文本。
// 绝不返回完整文件内容或工具输出。
func SummarizeToolInput(raw json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err == nil {
		if s := summarizeAskUser(m); s != "" {
			return Summarize(s, MaxCommandRunes)
		}
		for _, key := range []string{"command", "file_path", "path", "pattern", "query", "url"} {
			if v, ok := m[key].(string); ok && strings.TrimSpace(v) != "" {
				return Summarize(v, MaxCommandRunes)
			}
		}
	}
	return Summarize(string(raw), MaxCommandRunes)
}

// summarizeAskUser 把 AskUserQuestion 的 tool_input 渲染成可读摘要。
// 输入形态 {"questions":[{"question","header","options":[{"label"},…]},…]}：
// 原样落 JSON 会在审批卡上整串代码。输出「问题 · 选项a / 选项b」，多问以
// 「；」相连；header 与 question 前缀重复时省 header。非 questions 形态
// 返回空串，走既有键位提取。
func summarizeAskUser(m map[string]any) string {
	qs, ok := m["questions"].([]any)
	if !ok || len(qs) == 0 {
		return ""
	}
	var out []string
	for _, qv := range qs {
		q, ok := qv.(map[string]any)
		if !ok {
			continue
		}
		question, _ := q["question"].(string)
		question = strings.TrimSpace(question)
		header, _ := q["header"].(string)
		var opts []string
		if os, ok := q["options"].([]any); ok {
			for _, ov := range os {
				om, ok := ov.(map[string]any)
				if !ok {
					continue
				}
				if l, ok := om["label"].(string); ok && strings.TrimSpace(l) != "" {
					opts = append(opts, strings.TrimSpace(l))
				}
			}
		}
		if question == "" && len(opts) == 0 {
			continue
		}
		var b strings.Builder
		if header = strings.TrimSpace(header); header != "" && !strings.Contains(question, header) {
			b.WriteString(header)
			b.WriteString("：")
		}
		b.WriteString(question)
		if len(opts) > 0 {
			b.WriteString(" · ")
			b.WriteString(strings.Join(opts, " / "))
		}
		out = append(out, b.String())
	}
	return strings.Join(out, "；")
}
