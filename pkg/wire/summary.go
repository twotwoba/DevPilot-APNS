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
const (
	// HookTimeoutSeconds 是写入 Claude Code / Codex hook 配置的硬超时。
	HookTimeoutSeconds = 300
	// ApprovalWaitSeconds 是 daemon 等待手机决策的时间。
	ApprovalWaitSeconds = 240
	// PendingTTLSeconds 是 relay 保留 pending approval 的时间。
	PendingTTLSeconds = 300
	// APNSApprovalExpirySeconds 是审批推送的 APNs 过期窗口。
	APNSApprovalExpirySeconds = 120
	// LiveActivityStaleSeconds 是 Live Activity 卡的 stale 时间。
	LiveActivityStaleSeconds = 600
)

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
// 优先取 command / file_path / path / pattern，其次退化到 JSON 文本。
// 绝不返回完整文件内容或工具输出。
func SummarizeToolInput(raw json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err == nil {
		for _, key := range []string{"command", "file_path", "path", "pattern", "query", "url"} {
			if v, ok := m[key].(string); ok && strings.TrimSpace(v) != "" {
				return Summarize(v, MaxCommandRunes)
			}
		}
	}
	return Summarize(string(raw), MaxCommandRunes)
}
