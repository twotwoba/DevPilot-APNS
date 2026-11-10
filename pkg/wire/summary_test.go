package wire

import (
	"encoding/json"
	"reflect"
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

// TestSummarizeToolInputPlan ExitPlanMode 的 plan 提取（2026-11-04 真机截图
// 事故第二根因：整串 JSON 落卡）。plan 分支优先级最高；换行即结构（markdown
// 标题/列表），压单行等于报废；JSON 的 \n 转义经解码已是真实换行。
func TestSummarizeToolInputPlan(t *testing.T) {
	in := `{"plan":"## Goal\n\n- ship it\n- no regressions"}`
	got := SummarizeToolInput(json.RawMessage(in))
	want := "## Goal\n\n- ship it\n- no regressions"
	if got != want {
		t.Fatalf("plan:\n got  %q\n want %q", got, want)
	}

	// 超帽截断带省略号;尾部空白先修剪不占帽
	long := strings.Repeat("x", MaxPlanRunes+50)
	got = SummarizeToolInput(json.RawMessage(`{"plan":"` + long + "\n\n" + `"}`))
	if len([]rune(got)) > MaxPlanRunes {
		t.Fatalf("plan must be truncated to %d runes, got %d", MaxPlanRunes, len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated plan should end with ellipsis, got %q", got)
	}

	// plan 空串不吃摘要:回落原文兜底路径
	if got := SummarizeToolInput(json.RawMessage(`{"plan":"   "}`)); !strings.Contains(got, "plan") {
		t.Fatalf("blank plan should fall back to raw JSON text, got %q", got)
	}
}

// TestExtractOptions 选项提取:恰好一问且选项齐整才出;多问/缺 label/超量
// 一律 nil——App 回落 Approve/Deny 二值,不出半残的选项钮。
func TestExtractOptions(t *testing.T) {
	single := `{"questions":[{"question":"Q","header":"H","options":[{"label":"JWT"},{"label":"Session cookie"}]}]}`
	if got := ExtractOptions(json.RawMessage(single)); !reflect.DeepEqual(got, []string{"JWT", "Session cookie"}) {
		t.Fatalf("single question:\n got  %#v\n want [JWT Session cookie]", got)
	}

	cases := []string{
		`{bad`,                                                              // 非法 JSON
		`{"command":"git status"}`,                                          // 非 questions 形态
		`{"questions":[]}`,                                                  // 空问
		`{"questions":[{"question":"Q1"},{"question":"Q2"}]}`,               // 多问
		`{"questions":[{"question":"Q","multiSelect":true,"options":[{"label":"a"}]}]}`,   // 多选
		`{"questions":[{"question":"Q","multi_select":true,"options":[{"label":"a"}]}]}`,  // 多选（蛇形）
		`{"questions":[{"options":[{"label":"a"}]}]}`,                       // 无题面（answers 无处落键）
		`{"questions":[{"question":"Q","options":[{"label":"a"},{"label":""}]}]}`, // 空 label
		`{"questions":[{"question":"Q","options":[]}]}`,                     // 零选项
		`{"questions":[{"question":"Q","options":[{"label":"a"},{"label":"b"},{"label":"c"},{"label":"d"},{"label":"e"},{"label":"f"},{"label":"g"}]}]}`, // 超 6 项
	}
	for _, in := range cases {
		if got := ExtractOptions(json.RawMessage(in)); got != nil {
			t.Fatalf("ExtractOptions(%q) = %#v, want nil", in, got)
		}
	}

	// 显式 multiSelect=false 仍可出选项
	if got := ExtractOptions(json.RawMessage(`{"questions":[{"question":"Q","multiSelect":false,"options":[{"label":"a"}]}]}`)); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("multiSelect=false: got %#v", got)
	}

	// 长 label 截到 MaxOptionRunes 且带省略号
	long := strings.Repeat("选", MaxOptionRunes+10)
	got := ExtractOptions(json.RawMessage(`{"questions":[{"question":"Q","options":[{"label":"` + long + `"}]}]}`))
	if len(got) != 1 || len([]rune(got[0])) > MaxOptionRunes || !strings.HasSuffix(got[0], "…") {
		t.Fatalf("long label should be truncated: %#v", got)
	}
}

// TestPlainPreview 通知预览不得露出 markdown 记号。
func TestPlainPreview(t *testing.T) {
	in := "## Goal\n\n- ship **it**\n- keep `state`\n\n```swift\nlet a = 1\n```\nSee [docs](https://example.com)."
	got := PlainPreview(in, 200)
	want := "Goal ship it keep state let a = 1 See docs."
	if got != want {
		t.Fatalf("PlainPreview:\n got  %q\n want %q", got, want)
	}
	if long := PlainPreview(strings.Repeat("a", 30), 10); len([]rune(long)) != 10 || !strings.HasSuffix(long, "…") {
		t.Fatalf("PlainPreview cap: %q", long)
	}
	// 畸形链接原样保留，不吞正文
	if got := PlainPreview("see [broken text", 50); got != "see [broken text" {
		t.Fatalf("malformed link: %q", got)
	}
}
