package wire

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSummarizeToolInputAskUser AskUserQuestion 的人话渲染:整串 JSON 落审批卡
// 的事故(2026-11-04 真机截图)根因是六键位一个不占,退化到原文兜底。
func TestSummarizeToolInputAskUser(t *testing.T) {
	// 单问:header + question + 两选项
	single := `{"questions":[{"question":"把 * 号挪到最右边,你指的是哪个?","header":"* 指哪个键","options":[{"label":"符号行的 * 字符键"},{"label":"数字行的 * 键"}]}]}`
	got := SummarizeToolInput(json.RawMessage(single))
	want := "* 指哪个键：把 * 号挪到最右边,你指的是哪个? · 符号行的 * 字符键 / 数字行的 * 键"
	if got != want {
		t.Fatalf("single question:\n got  %q\n want %q", got, want)
	}

	// header 已含在 question 文本里 → 省前缀不重复
	dup := `{"questions":[{"question":"* 指哪个键?","header":"* 指哪个键","options":[{"label":"A"}]}]}`
	if got := SummarizeToolInput(json.RawMessage(dup)); got != "* 指哪个键? · A" {
		t.Fatalf("header dedup: got %q", got)
	}

	// 多问以「；」相连;无选项的问只出问题文本
	multi := `{"questions":[` +
		`{"question":"Q1","header":"H1","options":[{"label":"a"},{"label":"b"},{"label":"c"}]},` +
		`{"question":"Q2 only"}]}`
	if got, want := SummarizeToolInput(json.RawMessage(multi)), "H1：Q1 · a / b / c；Q2 only"; got != want {
		t.Fatalf("multi:\n got  %q\n want %q", got, want)
	}

	// 全空 questions → 回落原文兜底(不该渲染成空串吃掉摘要)
	empty := `{"questions":[]}`
	if got := SummarizeToolInput(json.RawMessage(empty)); !strings.Contains(got, "questions") {
		t.Fatalf("empty questions should fall back to raw JSON text, got %q", got)
	}

	// 非 questions 形态不受影响:既有键位提取仍然先行
	cmd := `{"command":"git status"}`
	if got := SummarizeToolInput(json.RawMessage(cmd)); got != "git status" {
		t.Fatalf("command key: got %q", got)
	}

	// 非法 JSON → 原文兜底路径保持
	if got := SummarizeToolInput(json.RawMessage(`{bad`)); got == "" {
		t.Fatal("invalid JSON should not produce empty summary")
	}
}

// TestSummarizeToolInputAskUserTruncation 摘要整体仍受 MaxCommandRunes 钳制
// (选项 label 很长的表单场景)。
func TestSummarizeToolInputAskUserTruncation(t *testing.T) {
	long := strings.Repeat("选项", 300)
	in := `{"questions":[{"question":"选一个","header":"H","options":[{"label":"` + long + `"}]}]}`
	got := SummarizeToolInput(json.RawMessage(in))
	if len([]rune(got)) > MaxCommandRunes {
		t.Fatalf("summary must be truncated to %d runes, got %d", MaxCommandRunes, len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated summary should end with ellipsis, got %q", got)
	}
}
