package proentitlement

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
	"strings"
	"testing"
	"time"
)

// —— 测试链生成:现场自签 root→intermediate→leaf(ECDSA P-256,ES256 签名用) ——

type testChain struct {
	rootPEM    []byte
	leaf       *x509.Certificate
	leafKey    *ecdsa.PrivateKey
	interDER64 string // x5c 第二张(base64(DER))
	leafDER64  string // x5c 第一张
}

func makeTestChain(t *testing.T, leafNotAfter time.Time) testChain {
	t.Helper()

	genCert := func(parent *x509.Certificate, parentKey *ecdsa.PrivateKey, tmpl *x509.Certificate) (*x509.Certificate, *ecdsa.PrivateKey) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		// 自签根:parent 传 nil 即模板自身(parentKey 同 key)
		if parent == nil {
			parent = tmpl
			parentKey = key
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
		if err != nil {
			t.Fatalf("create certificate: %v", err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatalf("parse certificate: %v", err)
		}
		return cert, key
	}

	caTmpl := func(cn string, isCA bool, notAfter time.Time) *x509.Certificate {
		tmpl := &x509.Certificate{
			SerialNumber:          big.NewInt(time.Now().UnixNano()),
			Subject:               pkix.Name{CommonName: cn, Organization: []string{"DevPilot Test"}},
			NotBefore:             time.Now().Add(-time.Hour),
			NotAfter:              notAfter,
			KeyUsage:              x509.KeyUsageDigitalSignature,
			ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
			BasicConstraintsValid: true,
		}
		if isCA {
			tmpl.IsCA = true
			tmpl.KeyUsage = x509.KeyUsageCertSign
		}
		return tmpl
	}

	now := time.Now()
	rootCert, rootKey := genCert(nil, nil, caTmpl("DevPilot Test Root", true, now.Add(10*365*24*time.Hour)))
	interCert, interKey := genCert(rootCert, rootKey, caTmpl("DevPilot Test Intermediate", true, now.Add(5*365*24*time.Hour)))
	leafCert, leafKey := genCert(interCert, interKey, caTmpl("DevPilot Test Leaf", false, leafNotAfter))

	rootDER, err := x509.CreateCertificate(rand.Reader, rootCert, rootCert, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatalf("re-encode root: %v", err)
	}
	rootPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER})

	return testChain{
		rootPEM:    rootPEM,
		leaf:       leafCert,
		leafKey:    leafKey,
		leafDER64:  base64.StdEncoding.EncodeToString(leafCert.Raw),
		interDER64: base64.StdEncoding.EncodeToString(interCert.Raw),
	}
}

// makeJWS 用给定链与 payload 组装一份 JWS(签名真做,camelCase 字段与 Apple 一致)。
func makeJWS(t *testing.T, tc testChain, payload map[string]any) string {
	t.Helper()

	header := map[string]any{"alg": "ES256", "x5c": []string{tc.leafDER64, tc.interDER64}}
	seg := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	h, p := seg(header), seg(payload)
	digest := sha256.Sum256([]byte(h + "." + p))
	r, s, err := ecdsa.Sign(rand.Reader, tc.leafKey, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return h + "." + p + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// defaultPayload 合法 Pro 年订阅交易的 payload(expires 未来一年)。
func defaultPayload(expires int64) map[string]any {
	return map[string]any{
		"transactionId":         "1000000123456789",
		"originalTransactionId": "1000000123456780",
		"bundleId":              "com.twotwoba.devpilot",
		"productId":             "com.devpilot.pro.annual",
		"environment":           "Production",
		"type":                  "Auto-Renewable Subscription",
		"purchaseDate":          time.Now().Add(-24 * time.Hour).UnixMilli(),
		"expiresDate":           expires,
		"signedDate":            time.Now().UnixMilli(),
	}
}

func newTestVerifier(t *testing.T, tc testChain, opts func(*Config)) *Verifier {
	t.Helper()
	cfg := Config{
		BundleID:     "com.twotwoba.devpilot",
		ProductIDs:   []string{"com.devpilot.pro.monthly", "com.devpilot.pro.annual", "com.devpilot.pro.lifetime"},
		Environments: []string{"Production"},
	}
	if opts != nil {
		opts(&cfg)
	}
	v, err := NewVerifierWithRoot(cfg, tc.rootPEM)
	if err != nil {
		t.Fatalf("NewVerifierWithRoot: %v", err)
	}
	return v
}

// —— 正路径 ——

func TestVerifyValidSubscription(t *testing.T) {
	tc := makeTestChain(t, time.Now().Add(365*24*time.Hour))
	v := newTestVerifier(t, tc, nil)
	ent, err := v.Verify(makeJWS(t, tc, defaultPayload(time.Now().Add(365*24*time.Hour).UnixMilli())))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if ent.ProductID != "com.devpilot.pro.annual" || ent.ExpiresDate == nil || ent.Environment != "PRODUCTION" {
		t.Fatalf("unexpected entitlement: %+v", ent)
	}
}

func TestVerifyValidLifetime(t *testing.T) {
	tc := makeTestChain(t, time.Now().Add(365*24*time.Hour))
	v := newTestVerifier(t, tc, nil)
	payload := defaultPayload(0)
	delete(payload, "expiresDate") // Non-Consumable 无 expiresDate 字段
	ent, err := v.Verify(makeJWS(t, tc, payload))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if ent.ExpiresDate != nil {
		t.Fatalf("lifetime should carry no expiry: %+v", ent)
	}
}

// —— payload 裁决拒绝路径 ——

func TestVerifyExpiredSubscription(t *testing.T) {
	tc := makeTestChain(t, time.Now().Add(365*24*time.Hour))
	v := newTestVerifier(t, tc, nil)
	_, err := v.Verify(makeJWS(t, tc, defaultPayload(time.Now().Add(-time.Hour).UnixMilli())))
	if Category(err) != errCatRejected || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("want expired rejection, got %v", err)
	}
}

func TestVerifyRevoked(t *testing.T) {
	tc := makeTestChain(t, time.Now().Add(365*24*time.Hour))
	v := newTestVerifier(t, tc, nil)
	payload := defaultPayload(time.Now().Add(30 * 24 * time.Hour).UnixMilli())
	payload["revocationDate"] = time.Now().UnixMilli()
	if _, err := v.Verify(makeJWS(t, tc, payload)); Category(err) != errCatRejected {
		t.Fatalf("want revoked rejection, got %v", err)
	}
}

func TestVerifyWrongBundle(t *testing.T) {
	tc := makeTestChain(t, time.Now().Add(365*24*time.Hour))
	v := newTestVerifier(t, tc, nil)
	payload := defaultPayload(time.Now().Add(30 * 24 * time.Hour).UnixMilli())
	payload["bundleId"] = "com.evil.clone"
	if _, err := v.Verify(makeJWS(t, tc, payload)); Category(err) != errCatRejected {
		t.Fatalf("want bundleId rejection, got %v", err)
	}
}

func TestVerifyProductNotInAllowlist(t *testing.T) {
	tc := makeTestChain(t, time.Now().Add(365*24*time.Hour))
	v := newTestVerifier(t, tc, nil)
	payload := defaultPayload(0)
	payload["productId"] = "com.devpilot.tip.jar"
	if _, err := v.Verify(makeJWS(t, tc, payload)); Category(err) != errCatRejected {
		t.Fatalf("want productId rejection, got %v", err)
	}
}

func TestVerifyEnvironmentPolicy(t *testing.T) {
	tc := makeTestChain(t, time.Now().Add(365*24*time.Hour))
	exp := time.Now().Add(30 * 24 * time.Hour).UnixMilli()

	// 生产验证器(仅 Production)拒 Sandbox
	v := newTestVerifier(t, tc, nil)
	payload := defaultPayload(exp)
	payload["environment"] = "Sandbox"
	if _, err := v.Verify(makeJWS(t, tc, payload)); Category(err) != errCatRejected {
		t.Fatalf("want environment rejection, got %v", err)
	}

	// staging 验证器(Production+Sandbox)收 Sandbox
	vs := newTestVerifier(t, tc, func(c *Config) { c.Environments = []string{"Production", "Sandbox"} })
	if _, err := vs.Verify(makeJWS(t, tc, payload)); err != nil {
		t.Fatalf("sandbox should pass on staging policy: %v", err)
	}
}

// —— 密码学拒绝路径 ——

func TestVerifyTamperedPayload(t *testing.T) {
	tc := makeTestChain(t, time.Now().Add(365*24*time.Hour))
	v := newTestVerifier(t, tc, nil)
	jws := makeJWS(t, tc, defaultPayload(time.Now().Add(30*24*time.Hour).UnixMilli()))
	parts := strings.Split(jws, ".")
	// 换 payload 为白名单外产品:签名对不上 → signature 类失败
	evil, _ := json.Marshal(map[string]any{
		"bundleId": "com.twotwoba.devpilot", "productId": "com.devpilot.pro.lifetime",
		"environment": "Production",
	})
	parts[1] = base64.RawURLEncoding.EncodeToString(evil)
	if _, err := v.Verify(strings.Join(parts, ".")); Category(err) != errCatSignature {
		t.Fatalf("want signature rejection, got %v", err)
	}
}

func TestVerifyForeignChain(t *testing.T) {
	// JWS 用链 A 签,验证器锚链 B → chain 失败
	mine := makeTestChain(t, time.Now().Add(365*24*time.Hour))
	foreign := makeTestChain(t, time.Now().Add(365*24*time.Hour))
	v := newTestVerifier(t, foreign, nil)
	if _, err := v.Verify(makeJWS(t, mine, defaultPayload(time.Now().Add(30*24*time.Hour).UnixMilli()))); Category(err) != errCatChain {
		t.Fatalf("want chain rejection, got %v", err)
	}
}

func TestVerifyExpiredLeafCertificate(t *testing.T) {
	// 叶子证书 NotAfter 已过 → chain 失败(时间窗口由 x509.Verify 裁决)
	tc := makeTestChain(t, time.Now().Add(-time.Hour))
	v := newTestVerifier(t, tc, nil)
	if _, err := v.Verify(makeJWS(t, tc, defaultPayload(time.Now().Add(30*24*time.Hour).UnixMilli()))); Category(err) != errCatChain {
		t.Fatalf("want chain rejection, got %v", err)
	}
}

// —— 结构性拒绝路径 ——

func TestVerifyMalformedJWS(t *testing.T) {
	tc := makeTestChain(t, time.Now().Add(365*24*time.Hour))
	v := newTestVerifier(t, tc, nil)
	cases := map[string]string{
		"two segments": "a.b",
		"empty":        "",
		"bad base64":   "!!!.!!!.!!!",
		"not json":     base64.RawURLEncoding.EncodeToString([]byte("junk")) + ".e30.igm",
	}
	for name, jws := range cases {
		if _, err := v.Verify(jws); Category(err) != errCatInvalid {
			t.Fatalf("%s: want malformed rejection, got %v", name, err)
		}
	}
}

func TestVerifyRejectsAlgNone(t *testing.T) {
	tc := makeTestChain(t, time.Now().Add(365*24*time.Hour))
	v := newTestVerifier(t, tc, nil)
	h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","x5c":["x","y"]}`))
	p := base64.RawURLEncoding.EncodeToString([]byte("{}"))
	if _, err := v.Verify(h + "." + p + ".igm"); Category(err) != errCatInvalid {
		t.Fatalf("want alg rejection, got %v", err)
	}
}

func TestNewVerifierRejectsEmptyConfig(t *testing.T) {
	if _, err := NewVerifier(Config{}); err == nil {
		t.Fatal("empty config must be rejected")
	}
}

// 内嵌 Apple Root CA - G3 必须始终可解析——生产信任锚,烂了等于验签全废。
func TestEmbeddedAppleRootLoads(t *testing.T) {
	v, err := NewVerifier(Config{
		BundleID:     "com.twotwoba.devpilot",
		ProductIDs:   []string{"com.devpilot.pro.annual"},
		Environments: []string{"Production"},
	})
	if err != nil {
		t.Fatalf("embedded Apple root failed to load: %v", err)
	}
	if v == nil {
		t.Fatal("nil verifier")
	}
}
