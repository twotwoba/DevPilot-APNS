// Package pairing 实现配对码、凭据与哈希派生。
package pairing

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

const (
	// CodeTTL 是配对码的有效期。
	CodeTTL = 15 * time.Minute
	// DaemonPrefix / DevicePrefix 是凭据前缀。
	DaemonPrefix = "dpd"
	DevicePrefix = "dpa"
	// DaemonIDPrefix / DeviceIDPrefix 是 ID 前缀，解析凭据时用于校验。
	DaemonIDPrefix = "dd_"
	DeviceIDPrefix = "dv_"
)

// NewCode 生成 128 位、无易混字符（base32）的配对码。
func NewCode() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("pairing: failed to generate pairing code: %w", err)
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]), nil
}

// CodeHash 返回配对码的存储哈希（hex sha256）。
func CodeHash(code string) string { return hashHex(code) }

// NewSecret 生成 256 位随机 secret。
func NewSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("pairing: failed to generate secret: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// SecretHash 返回 secret 的存储哈希。
func SecretHash(secret string) string { return hashHex(secret) }

// NewDaemonID 生成 daemon id（正常由 daemon 自己生成，仅测试/兜底用）。
func NewDaemonID() string { return DaemonIDPrefix + randomHex(16) }

// NewDeviceID 生成 device id。
func NewDeviceID() string { return DeviceIDPrefix + randomHex(16) }

// DaemonCredential 组装 daemon 凭据。
func DaemonCredential(daemonID, secret string) string {
	return DaemonPrefix + "_" + daemonID + "_" + secret
}

// DeviceCredential 组装 device 凭据。
func DeviceCredential(deviceID, secret string) string {
	return DevicePrefix + "_" + deviceID + "_" + secret
}

// ParseCredential 解析 `dpd_<id>_<secret>` / `dpa_<id>_<secret>`。
//
// id 自身含下划线（dd_/dv_），因此按最后一个下划线切分 secret。
func ParseCredential(token, prefix, idPrefix string) (id, secret string, ok bool) {
	head := prefix + "_"
	if !strings.HasPrefix(token, head) {
		return "", "", false
	}
	rest := token[len(head):]
	i := strings.LastIndex(rest, "_")
	if i <= 0 || i == len(rest)-1 {
		return "", "", false
	}
	id, secret = rest[:i], rest[i+1:]
	if !strings.HasPrefix(id, idPrefix) || len(secret) < 32 {
		return "", "", false
	}
	return id, secret, true
}

// VerifyHash 以常量时间比较 secret 与其存储哈希。
func VerifyHash(secret, storedHash string) bool {
	return subtle.ConstantTimeCompare([]byte(SecretHash(secret)), []byte(storedHash)) == 1
}

func hashHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("pairing: system random source unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}
