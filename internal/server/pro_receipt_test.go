package server

// Pro 档位收据验签的端点级回归:
// - 自建部署(verifier nil)自报直通——v1 行为回归钉;
// - 验签部署:自报 pro 缺失/伪造凭证降 free(配对照常成功),有效凭证落 pro;
// - token 改档升 pro 须过闸,422 不写任何库(半提交防线),降 free 恒放行。
// 验签本体细节(链/签名/payload 裁决)由 proentitlement 包测试覆盖,这里
// 只用自签测试链当「能通过验签的凭证」供给方。

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/twotwoba/DevPilot-APNS/internal/pairing"
	"github.com/twotwoba/DevPilot-APNS/internal/proentitlement"
	"github.com/twotwoba/DevPilot-APNS/internal/store"
)

// buildProFixture 自签 root+leaf 两级链,返回锚定该根的验证器与有效凭证工厂。
func buildProFixture(t *testing.T) (*proentitlement.Verifier, func() string) {
	t.Helper()

	mkKey := func() *ecdsa.PrivateKey {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		return key
	}
	rootKey := mkKey()
	rootTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "DevPilot Server Test Root"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTmpl, rootTmpl, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	rootCert, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatalf("parse root: %v", err)
	}
	leafKey := mkKey()
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "DevPilot Server Test Leaf"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, rootCert, &leafKey.PublicKey, rootKey)
	if err != nil {
		t.Fatalf("create leaf: %v", err)
	}
	leafCert, err := x509.ParseCertificate(leafDER)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}

	verifier, err := proentitlement.NewVerifierWithRoot(proentitlement.Config{
		BundleID:     "com.twotwoba.devpilot",
		ProductIDs:   []string{"com.devpilot.pro.monthly", "com.devpilot.pro.annual", "com.devpilot.pro.lifetime"},
		Environments: []string{"Production"},
	}, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER}))
	if err != nil {
		t.Fatalf("build verifier: %v", err)
	}

	validJWS := func() string {
		return signTestJWS(t, leafKey, [][]byte{leafCert.Raw, rootCert.Raw}, map[string]any{
			"transactionId": "1000000123456789",
			"bundleId":      "com.twotwoba.devpilot",
			"productId":     "com.devpilot.pro.annual",
			"environment":   "Production",
			"purchaseDate":  time.Now().Add(-24 * time.Hour).UnixMilli(),
			"expiresDate":   time.Now().Add(30 * 24 * time.Hour).UnixMilli(),
			"signedDate":    time.Now().UnixMilli(),
		})
	}
	return verifier, validJWS
}

// signTestJWS 组装并真签一份 JWS(header x5c 为 base64(DER),签名 ES256 raw R||S)。
func signTestJWS(t *testing.T, key *ecdsa.PrivateKey, x5cDERs [][]byte, payload map[string]any) string {
	t.Helper()

	x5c := make([]string, 0, len(x5cDERs))
	for _, der := range x5cDERs {
		x5c = append(x5c, base64.StdEncoding.EncodeToString(der))
	}
	seg := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	h, p := seg(map[string]any{"alg": "ES256", "x5c": x5c}), seg(payload)
	digest := sha256.Sum256([]byte(h + "." + p))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return h + "." + p + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// pairWithTier 登记配对会话并以给定 tier/pro_jws 兑换,返回状态码与响应体。
func (e *testEnv) pairWithTier(t *testing.T, tier, proJWS string) (int, map[string]any) {
	t.Helper()
	resp, body := e.post(t, "/v1/pairing/sessions", map[string]any{
		"code": testCode, "daemon_id": testDaemonID,
		"daemon_cred_hash": pairing.SecretHash(testSecret),
		"daemon_name":      "Eric's MacBook",
		"expires_at":       time.Now().Add(10 * time.Minute).Unix(),
	}, "")
	if resp.StatusCode != 201 {
		t.Fatalf("register pairing session HTTP %d: %s", resp.StatusCode, body)
	}
	req := map[string]any{
		"code": testCode, "apns_token": "fake-token", "device_name": "iPhone",
	}
	if tier != "" {
		req["tier"] = tier
	}
	if proJWS != "" {
		req["pro_jws"] = proJWS
	}
	resp, body = e.post(t, "/v1/pair", req, "")
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	// 回填设备身份,后续 token 端点测试按凭据调用
	if resp.StatusCode == 201 {
		e.deviceID, _ = out["device_id"].(string)
		e.deviceCr, _ = out["device_credential"].(string)
	}
	return resp.StatusCode, out
}

// daemonTier 读库中 daemon 的当前档位。
func (e *testEnv) daemonTier(t *testing.T) store.Tier {
	t.Helper()
	d, err := e.store.Daemon(testDaemonID)
	if err != nil {
		t.Fatalf("load daemon: %v", err)
	}
	return d.Tier
}

// —— pair 端点 ——

// 自建部署回归:verifier 为 nil 时自报 pro 直通(v1 行为,不变)。
func TestPairTierUnenforcedClaimsPro(t *testing.T) {
	env := newTestEnv(t)
	status, out := env.pairWithTier(t, "pro", "")
	if status != 201 {
		t.Fatalf("pair HTTP %d: %v", status, out)
	}
	if out["tier"] != "pro" {
		t.Fatalf("unenforced should honor claimed pro, got %v", out["tier"])
	}
	if env.daemonTier(t) != store.TierPro {
		t.Fatalf("daemon tier should be pro, got %s", env.daemonTier(t))
	}
}

// 验签部署:自报 pro 无凭证 → 配对照常成功,降 free(配对永不因档位失败)。
func TestPairEnforcedProWithoutReceiptDowngrades(t *testing.T) {
	verifier, _ := buildProFixture(t)
	env := newTestEnvOpts(t, testEnvOpts{ProVerifier: verifier})
	status, out := env.pairWithTier(t, "pro", "")
	if status != 201 {
		t.Fatalf("pair HTTP %d: %v", status, out)
	}
	if out["tier"] != "free" {
		t.Fatalf("missing receipt should downgrade to free, got %v", out["tier"])
	}
	if env.daemonTier(t) != store.TierFree {
		t.Fatalf("daemon tier should be free, got %s", env.daemonTier(t))
	}
}

// 验签部署:有效 Apple 签名凭证 → 落 pro。
func TestPairEnforcedProWithValidReceipt(t *testing.T) {
	verifier, validJWS := buildProFixture(t)
	env := newTestEnvOpts(t, testEnvOpts{ProVerifier: verifier})
	status, out := env.pairWithTier(t, "pro", validJWS())
	if status != 201 {
		t.Fatalf("pair HTTP %d: %v", status, out)
	}
	if out["tier"] != "pro" {
		t.Fatalf("valid receipt should grant pro, got %v (resp %v)", out["tier"], out)
	}
	if env.daemonTier(t) != store.TierPro {
		t.Fatalf("daemon tier should be pro, got %s", env.daemonTier(t))
	}
}

// 验签部署:伪造/垃圾凭证 → 降 free。
func TestPairEnforcedProWithGarbageReceipt(t *testing.T) {
	verifier, _ := buildProFixture(t)
	env := newTestEnvOpts(t, testEnvOpts{ProVerifier: verifier})
	status, out := env.pairWithTier(t, "pro", "eyJhbGciOiJub25lIn0.e30.AAAA")
	if status != 201 {
		t.Fatalf("pair HTTP %d: %v", status, out)
	}
	if env.daemonTier(t) != store.TierFree {
		t.Fatalf("garbage receipt should downgrade, got %s", env.daemonTier(t))
	}
}

// —— token 端点(权益同步) ——

// 升 pro 无凭证 → 422,且不写任何库(token/tier 都不碰——半提交防线)。
func TestDeviceTokenUpgradeWithoutReceiptRejected(t *testing.T) {
	verifier, _ := buildProFixture(t)
	env := newTestEnvOpts(t, testEnvOpts{ProVerifier: verifier})
	if status, _ := env.pairWithTier(t, "", ""); status != 201 {
		t.Fatalf("pair HTTP %d", status)
	}
	// 记住配对时的 token:422 必须连 token 更新一起回滚
	resp, body := env.post(t, "/v1/devices/token", map[string]any{
		"apns_token": "new-token", "tier": "pro",
	}, env.deviceCr)
	if resp.StatusCode != 422 {
		t.Fatalf("upgrade without receipt: want 422, got %d: %s", resp.StatusCode, body)
	}
	if env.daemonTier(t) != store.TierFree {
		t.Fatalf("tier must stay free, got %s", env.daemonTier(t))
	}
	dev, err := env.store.Device(env.deviceID)
	if err != nil {
		t.Fatal(err)
	}
	if dev.APNSToken != "fake-token" {
		t.Fatalf("token write must be rolled back on receipt rejection, got %q", dev.APNSToken)
	}
}

// 升 pro 带有效凭证 → 204 落 pro;降 free 无凭证恒放行。
func TestDeviceTokenUpgradeWithReceiptAndDowngrade(t *testing.T) {
	verifier, validJWS := buildProFixture(t)
	env := newTestEnvOpts(t, testEnvOpts{ProVerifier: verifier})
	if status, _ := env.pairWithTier(t, "", ""); status != 201 {
		t.Fatalf("pair HTTP %d", status)
	}

	resp, body := env.post(t, "/v1/devices/token", map[string]any{
		"apns_token": "new-token", "tier": "pro", "pro_jws": validJWS(),
	}, env.deviceCr)
	if resp.StatusCode != 204 {
		t.Fatalf("upgrade with receipt: want 204, got %d: %s", resp.StatusCode, body)
	}
	if env.daemonTier(t) != store.TierPro {
		t.Fatalf("daemon tier should be pro, got %s", env.daemonTier(t))
	}

	resp, _ = env.post(t, "/v1/devices/token", map[string]any{
		"apns_token": "newer-token", "tier": "free",
	}, env.deviceCr)
	if resp.StatusCode != 204 {
		t.Fatalf("downgrade must never be blocked, got %d", resp.StatusCode)
	}
	if env.daemonTier(t) != store.TierFree {
		t.Fatalf("daemon tier should be free, got %s", env.daemonTier(t))
	}
}

// 伪造凭证升 pro → 422。
func TestDeviceTokenUpgradeWithBadReceiptRejected(t *testing.T) {
	verifier, _ := buildProFixture(t)
	env := newTestEnvOpts(t, testEnvOpts{ProVerifier: verifier})
	if status, _ := env.pairWithTier(t, "", ""); status != 201 {
		t.Fatalf("pair HTTP %d", status)
	}
	resp, _ := env.post(t, "/v1/devices/token", map[string]any{
		"apns_token": "new-token", "tier": "pro", "pro_jws": "a.b.c",
	}, env.deviceCr)
	if resp.StatusCode != 422 {
		t.Fatalf("bad receipt upgrade: want 422, got %d", resp.StatusCode)
	}
}
