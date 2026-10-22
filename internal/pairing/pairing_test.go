package pairing

import (
	"strings"
	"testing"
)

func TestDaemonCredentialRoundTrip(t *testing.T) {
	cred := DaemonCredential("dd_abc123", "secretsecretsecretsecretsecret12")
	id, secret, ok := ParseCredential(cred, DaemonPrefix, DaemonIDPrefix)
	if !ok || id != "dd_abc123" || secret != "secretsecretsecretsecretsecret12" {
		t.Fatalf("解析结果 id=%q secret=%q ok=%v", id, secret, ok)
	}
	if !VerifyHash(secret, SecretHash(secret)) {
		t.Fatal("哈希校验失败")
	}
	if VerifyHash(secret, SecretHash("other")) {
		t.Fatal("错误 secret 不应通过")
	}
}

func TestParseCredentialRejectsMalformed(t *testing.T) {
	cases := []string{
		"",
		"dpd_",
		"dpd_dd_x",
		"dpa_dd_abc_secretsecretsecretsecretsecret",
		"dpd_dv_abc_secretsecretsecretsecretsecret",
		"dpd_dd_abc_short",
	}
	for _, c := range cases {
		if _, _, ok := ParseCredential(c, DaemonPrefix, DaemonIDPrefix); ok {
			t.Fatalf("不应接受 %q", c)
		}
	}
}

func TestDeviceCredentialRoundTrip(t *testing.T) {
	cred := DeviceCredential("dv_abc123", strings.Repeat("s", 32))
	id, secret, ok := ParseCredential(cred, DevicePrefix, DeviceIDPrefix)
	if !ok || id != "dv_abc123" || len(secret) != 32 {
		t.Fatalf("解析失败: %q %q %v", id, secret, ok)
	}
}

func TestNewCodeUniqueAndValid(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		code, err := NewCode()
		if err != nil {
			t.Fatal(err)
		}
		if len(code) != 26 {
			t.Fatalf("配对码长度 = %d", len(code))
		}
		for _, r := range code {
			if !(r >= 'A' && r <= 'Z') && !(r >= '2' && r <= '7') {
				t.Fatalf("配对码含非法字符 %q", r)
			}
		}
		if seen[code] {
			t.Fatal("配对码重复")
		}
		seen[code] = true
	}
}
