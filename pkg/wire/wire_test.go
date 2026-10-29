package wire

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestVectorsRoundTrip 保证契约向量可以解码、通过校验、重新编码后语义不变。
// iOS 侧应加载同一批文件做 Codable 测试。
func TestVectorsRoundTrip(t *testing.T) {
	dir := filepath.Join("testdata", "vectors")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取向量目录失败: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("向量目录为空")
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		t.Run(entry.Name(), func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatalf("读取向量失败: %v", err)
			}
			first, err := Decode(data)
			if err != nil {
				t.Fatalf("解码失败: %v", err)
			}
			encoded, err := Encode(first)
			if err != nil {
				t.Fatalf("编码失败: %v", err)
			}
			second, err := Decode(encoded)
			if err != nil {
				t.Fatalf("二次解码失败: %v", err)
			}
			// 直接比较结构体会把 nil 切片与空切片误判为不同；
			// 比较重新编码后的字节既能覆盖字段，也能覆盖序列化稳定性。
			reencoded, err := Encode(second)
			if err != nil {
				t.Fatalf("二次编码失败: %v", err)
			}
			if !reflect.DeepEqual(encoded, reencoded) {
				t.Fatalf("往返后不一致:\n第一次: %s\n第二次: %s", encoded, reencoded)
			}
		})
	}
}

// TestDecodeRejectsUnsupportedVersion 确认版本不匹配时直接拒绝。
func TestDecodeRejectsUnsupportedVersion(t *testing.T) {
	_, err := Decode([]byte(`{"v":2,"kind":"event"}`))
	if err == nil || !strings.Contains(err.Error(), "unsupported protocol version") {
		t.Fatalf("期望版本错误，实际 %v", err)
	}
}

// TestDecodeRejectsUnknownKind 确认未知 kind 不会被静默接受。
func TestDecodeRejectsUnknownKind(t *testing.T) {
	if _, err := Decode([]byte(`{"v":1,"kind":"whatever"}`)); err == nil {
		t.Fatal("期望未知 kind 报错")
	}
}

// TestApprovalRequiredNeedsID 确认审批事件必须带 approval_id。
func TestApprovalRequiredNeedsID(t *testing.T) {
	env := Envelope{
		V: Version, Kind: KindEvent, MsgID: "m", EventID: "e",
		Type: EventApprovalRequired, Agent: AgentClaude,
		DaemonID: "dd_x", SessionID: "s", TS: 1,
	}
	if err := env.Validate(); err == nil {
		t.Fatal("缺少 approval_id 时应报错")
	}
}

// TestApprovalIDDeterministic 确认键顺序与空白不影响审批 ID。
func TestApprovalIDDeterministic(t *testing.T) {
	a, err := ApprovalID("sess", "Bash", "tu_1", json.RawMessage(`{"command":"ls","cwd":"/tmp"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := ApprovalID("sess", "Bash", "tu_1", json.RawMessage("{\n  \"cwd\": \"/tmp\",\n  \"command\": \"ls\"\n}"))
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("审批 ID 应确定: %s != %s", a, b)
	}
	if !strings.HasPrefix(a, "apv_") || len(a) != 4+26 {
		t.Fatalf("审批 ID 格式不符: %q", a)
	}

	c, _ := ApprovalID("sess", "Bash", "tu_2", json.RawMessage(`{"command":"ls"}`))
	if a == c {
		t.Fatal("不同 tool_use_id 不应产生相同 ID")
	}
}

// TestCanonicalJSONPreservesNumbers 确认大整数不会被浮点化。
func TestCanonicalJSONPreservesNumbers(t *testing.T) {
	out, err := CanonicalJSON([]byte(`{"b":9007199254740993,"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"a":1,"b":9007199254740993}` {
		t.Fatalf("规范化结果不符: %s", out)
	}
}

// TestCanonicalJSONRejectsTrailing 确认多余内容被拒绝。
func TestCanonicalJSONRejectsTrailing(t *testing.T) {
	if _, err := CanonicalJSON([]byte(`{} {}`)); err == nil {
		t.Fatal("期望尾随内容报错")
	}
}

// TestTruncateCountsRunes 确认按字符而非字节截断。
func TestTruncateCountsRunes(t *testing.T) {
	s := strings.Repeat("中", 10)
	got := Truncate(s, 5)
	if len([]rune(got)) != 5 || !strings.HasSuffix(got, "…") {
		t.Fatalf("截断结果不符: %q", got)
	}
	if Truncate("短", 5) != "短" {
		t.Fatal("未超限不应改动")
	}
}

// TestContentStateUsesEpochSeconds 固化 Live Activity 时间契约。
func TestContentStateUsesEpochSeconds(t *testing.T) {
	env := Envelope{
		Type:      EventApprovalRequired,
		SessionID: "sess_0123456789",
		Payload:   Payload{SessionTitle: "重构支付模块"},
		TS:        1760770000,
	}
	cs := ContentStateFor(env)
	if cs.UpdatedAt != 1760770000 {
		t.Fatalf("updatedAt 必须是 Unix 秒，实际 %d", cs.UpdatedAt)
	}
	if !cs.IsAlert || cs.StatusText != "Waiting for approval" {
		t.Fatalf("审批态映射不符: %+v", cs)
	}

	// 序列化后必须是整数，不能是字符串或浮点。
	raw, err := json.Marshal(cs)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["updatedAt"].(float64); !ok {
		t.Fatalf("updatedAt 应为 JSON 数字: %s", raw)
	}
}

// TestLiveActivityEndsOnSessionEnd 确认结束事件映射为 end 并带 dismissal。
func TestLiveActivityEndsOnSessionEnd(t *testing.T) {
	push := LiveActivityPushFor(Envelope{Type: EventSessionEnded, SessionID: "s", TS: 100}, 200)
	if push.APS.Event != "end" {
		t.Fatalf("期望 end，实际 %q", push.APS.Event)
	}
	if push.APS.DismissalDate != 200+LiveActivityStaleSeconds {
		t.Fatalf("dismissal-date 不符: %d", push.APS.DismissalDate)
	}

	update := LiveActivityPushFor(Envelope{Type: EventToolRunning, SessionID: "s", TS: 100}, 200)
	if update.APS.Event != "update" {
		t.Fatalf("期望 update，实际 %q", update.APS.Event)
	}
}

// TestHelloValidation 确认角色与身份字段的约束。
func TestHelloValidation(t *testing.T) {
	ok := Hello{V: Version, Kind: KindHello, MsgID: "m", Role: RoleApp, DeviceID: "d"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("合法 hello 被拒: %v", err)
	}
	bad := Hello{V: Version, Kind: KindHello, MsgID: "m", Role: RoleDaemon}
	if err := bad.Validate(); err == nil {
		t.Fatal("daemon 缺少 daemon_id 应报错")
	}
}
