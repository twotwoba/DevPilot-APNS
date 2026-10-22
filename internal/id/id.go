// Package id 生成 relay 侧的消息 id。
package id

import (
	"crypto/rand"
	"encoding/hex"
)

// New 返回一条消息的幂等键。
func New() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("id: 系统随机源不可用: " + err.Error())
	}
	return "m_" + hex.EncodeToString(b[:])
}
