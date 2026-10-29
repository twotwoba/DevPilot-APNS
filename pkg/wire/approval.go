package wire

import (
	"bytes"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// ApprovalID 生成确定性的审批 ID。
//
// 同一个工具调用（session + tool + tool_use_id + 规范化输入）总是得到同一个 ID：
// 这样 hook 被重复触发时不会在手机上产生两条审批。tool_use_id 缺失时调用方应
// 退化为 UUID，并接受极端重复的可能。
func ApprovalID(sessionID, toolName, toolUseID string, toolInput json.RawMessage) (string, error) {
	canonical, err := CanonicalJSON(toolInput)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	writeField(h, sessionID)
	writeField(h, toolName)
	writeField(h, toolUseID)
	h.Write(canonical)
	sum := h.Sum(nil)
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum)
	return "apv_" + strings.ToLower(enc)[:26], nil
}

func writeField(w io.Writer, s string) {
	_, _ = io.WriteString(w, s)
	_, _ = w.Write([]byte{0})
}

// CanonicalJSON 把任意 JSON 规范化为稳定字节序列。
//
// 规则：对象键排序、数字保留原始字面量（UseNumber）、忽略空白。
// 空输入视为 {}。
func CanonicalJSON(raw []byte) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return []byte("{}"), nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("wire: failed to parse tool input: %w", err)
	}
	// 拒绝尾随的第二个 JSON 值，避免「看到的」与「哈希的」不一致。
	if dec.More() {
		return nil, fmt.Errorf("wire: tool input contains trailing data")
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("wire: failed to canonicalize tool input: %w", err)
	}
	return out, nil
}

// InputDigest 返回规范化工具输入的短哈希，用于展示与防篡改。
func InputDigest(raw json.RawMessage) string {
	canonical, err := CanonicalJSON(raw)
	if err != nil {
		canonical = []byte("{}")
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])[:16]
}
