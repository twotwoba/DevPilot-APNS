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
	// MaxPlanRunes 是 ExitPlanMode plan 详情帽。方案是用户要在手机上读后
	// 拍板的正文，256 的横幅帽必出残篇；对齐 ReplySummaryRunes 的详情口径
	// （保换行保结构，App 端 markdown 渲染），列表预览由 App 侧自行截断。
	MaxPlanRunes = 2000
	// MaxOptionRunes 是单个选项标签的帽。AskUserQuestion 的 label 设计上
	// 就是短词（5-8 词），80 runes 是防御位不是常态。
	MaxOptionRunes = 80
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

// SummarizeToolInput 从 tool_input 中提取用户可读的摘要。
//
// 优先级：ExitPlanMode 的 plan（保换行的 markdown 正文，详情帽）→
// AskUserQuestion 的 questions 数组（渲染成人话，见 summarizeAskUser）→
// command / file_path / notebook_path / path / pattern / query / url 等键位
// 单行提取，最后退化到 JSON 文本单行。绝不返回完整文件内容或工具输出。
func SummarizeToolInput(raw json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err == nil {
		// plan 是要拍板的方案正文：换行即结构（标题/列表/栅栏），压成单行
		// 等于报废。直取字符串值——JSON 里的 \n 转义经解码已是真实换行，
		// 「卡片上一坨转义符」的观感随整串 JSON 落卡一起消失。
		if plan, ok := m["plan"].(string); ok && strings.TrimSpace(plan) != "" {
			return Truncate(strings.TrimRight(plan, " \t\n"), MaxPlanRunes)
		}
		if s := summarizeAskUser(m); s != "" {
			return Summarize(s, MaxCommandRunes)
		}
		for _, key := range []string{"command", "file_path", "notebook_path", "path", "pattern", "query", "url"} {
			if v, ok := m[key].(string); ok && strings.TrimSpace(v) != "" {
				return Summarize(v, MaxCommandRunes)
			}
		}
	}
	return Summarize(string(raw), MaxCommandRunes)
}

// ExtractOptions 提取 AskUserQuestion 的候选标签，供审批卡渲染选项钮。
//
// 只在「恰好一问、每问选项齐整」时返回非 nil：多问时手机一次只能替用户
// 答一问，回退 Approve/Deny 二值更诚实。标签顺序即 JSON 顺序（App 渲染
// 与回传选择都按此序）；单个标签截到 MaxOptionRunes。
func ExtractOptions(raw json.RawMessage) []string {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	qs, ok := m["questions"].([]any)
	if !ok || len(qs) != 1 {
		return nil
	}
	q, ok := qs[0].(map[string]any)
	if !ok {
		return nil
	}
	os, ok := q["options"].([]any)
	if !ok || len(os) == 0 || len(os) > 6 {
		return nil
	}
	out := make([]string, 0, len(os))
	for _, ov := range os {
		om, ok := ov.(map[string]any)
		if !ok {
			return nil
		}
		l, _ := om["label"].(string)
		l = strings.TrimSpace(l)
		if l == "" {
			return nil
		}
		out = append(out, Truncate(l, MaxOptionRunes))
	}
	return out
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
