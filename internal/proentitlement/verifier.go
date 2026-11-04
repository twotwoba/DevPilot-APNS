// Package proentitlement 验证 StoreKit 2 交易 JWS(Apple 签名的 Pro 权益凭证)。
//
// 背景:tier 原为客户端自声明(store.Tier 注释明示"收据验证属后续硬化"),
// 官方 relay 上免费用户改一下输入(或抓包改字段)即可白嫖 pro 配额。本包把
// 「pro 档位必须持有 Apple 签名的有效交易凭证」落到服务端:App 上报
// Transaction.currentEntitlements 里 verified 事务的 jwsRepresentation,
// 服务端验 Apple 证书链 + ES256 签名 + payload 裁决(bundleID/产品白名单/
// 未过期未撤销/environment)。
//
// 验签模型(App Store Server JWS 标准):
//
//	header  { "alg":"ES256", "x5c":[leaf, intermediate, root] }   // x5c 为 base64(DER)
//	payload { "productId","bundleId","expiresDate","revocationDate","environment",... }  // camelCase,时间为毫秒
//	signature ES256 raw R||S(64 字节)over header.payload
//
// 信任锚:Apple Root CA - G3(testdata/AppleRootCA-G3.pem 内嵌,
// sha256 63343ABFB89A6A03EBB57E9B3F5FA7BE7C4F5C756F3017B3A8C488C3653E9179,
// Apple 公布的 App Store Server JWS 验证根,自签、2039 到期)。
// 伪造 JWS 需要苹果签名证书私钥——OCSP 吊销检查(防 Apple 侧轮换场景)列为
// 后续硬化,不在本期。
package proentitlement

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

//go:embed testdata/AppleRootCA-G3.pem
var appleRootG3PEM []byte

// Config 验签裁决参数。零值不可用,必须经 NewVerifier 构造。
type Config struct {
	// BundleID 交易必须归属的 App(与 APNS_BUNDLE_ID 同源)。
	BundleID string
	// ProductIDs 允许落 pro 的产品白名单(月/年订阅 + 买断)。
	ProductIDs []string
	// Environments 允许的交易环境("Production"/"Sandbox"/"Xcode" 子集)。
	// 生产部署默认仅 Production;staging 可放 Sandbox(TestFlight 内购走 Sandbox)。
	Environments []string
}

// Entitlement 验签通过后的权益事实。
type Entitlement struct {
	ProductID   string
	ExpiresDate *time.Time // 订阅到期;nil=买断(无到期概念)
	Environment string
}

// Verifier 并发安全(AppleRootCA-G3 只读)。
type Verifier struct {
	rootPool *x509.CertPool
	bundleID string
	products map[string]bool
	envs     map[string]bool
	now      func() time.Time
}

// NewVerifier 用内嵌 Apple Root CA - G3 构造生产验证器。
func NewVerifier(cfg Config) (*Verifier, error) {
	return NewVerifierWithRoot(cfg, appleRootG3PEM)
}

// NewVerifierWithRoot 用给定 PEM 根构造验证器。server 包的集成测试注入
// 自签测试链;生产一律走 NewVerifier(内嵌 Apple 根)。
func NewVerifierWithRoot(cfg Config, rootPEM []byte) (*Verifier, error) {
	if cfg.BundleID == "" {
		return nil, errors.New("proentitlement: BundleID is required")
	}
	if len(cfg.ProductIDs) == 0 || len(cfg.Environments) == 0 {
		return nil, errors.New("proentitlement: ProductIDs and Environments are required")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(rootPEM) {
		return nil, errors.New("proentitlement: root PEM contains no certificates")
	}
	v := &Verifier{
		rootPool: pool,
		bundleID: cfg.BundleID,
		products: make(map[string]bool, len(cfg.ProductIDs)),
		envs:     make(map[string]bool, len(cfg.Environments)),
		now:      time.Now,
	}
	for _, id := range cfg.ProductIDs {
		v.products[id] = true
	}
	for _, e := range cfg.Environments {
		v.envs[strings.ToUpper(strings.TrimSpace(e))] = true
	}
	return v, nil
}

// Verify 校验一份 StoreKit 2 交易 JWS,返回权益事实。任何失败返回
// 描述性 error(日志用;HTTP 侧对外统一文案,不回显细节)。
func (v *Verifier) Verify(jws string) (Entitlement, error) {
	parts := strings.Split(strings.TrimSpace(jws), ".")
	if len(parts) != 3 {
		return Entitlement{}, errInvalid("jws must have 3 segments")
	}

	// —— header:锁 alg=ES256,取 x5c 证书链 ——
	var header struct {
		Alg string   `json:"alg"`
		X5C []string `json:"x5c"`
	}
	headerJSON, err := decodeSegment(parts[0])
	if err != nil {
		return Entitlement{}, errInvalid("bad header encoding")
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return Entitlement{}, errInvalid("bad header json")
	}
	if header.Alg != "ES256" {
		// alg 混淆防御:只接受 Apple 实际使用的 ES256,其余一律拒
		return Entitlement{}, errInvalid("unsupported alg " + header.Alg)
	}
	if len(header.X5C) < 2 {
		return Entitlement{}, errInvalid("x5c chain too short")
	}
	certs := make([]*x509.Certificate, 0, len(header.X5C))
	for i, b64 := range header.X5C {
		der, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return Entitlement{}, errInvalid(fmt.Sprintf("x5c[%d] not base64", i))
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return Entitlement{}, errInvalid(fmt.Sprintf("x5c[%d] not a certificate", i))
		}
		certs = append(certs, cert)
	}
	leaf := certs[0]

	// —— 证书链:叶子必须锚定 Apple Root CA - G3,且验证时刻在有效窗口内 ——
	inters := x509.NewCertPool()
	for _, inter := range certs[1:] {
		inters.AddCert(inter)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:         v.rootPool,
		Intermediates: inters,
		CurrentTime:   v.now(),
		// KeyUsages 必须显式 Any:缺省值是 ServerAuth,而 Apple 签名证书带
		// codeSigning/苹果自定义 OID——缺省直接判链失败;信任锚是根证书,
		// EKU 不参与裁决
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}); err != nil {
		return Entitlement{}, errChain("certificate chain verification failed")
	}

	// —— ES256 签名:JWS raw R||S → Go 大整数对,摘要为 SHA256(signing input) ——
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Entitlement{}, errInvalid("bad signature encoding")
	}
	if len(sig) != 64 {
		return Entitlement{}, errInvalid("signature must be 64-byte raw R||S")
	}
	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return Entitlement{}, errChain("leaf certificate carries no ECDSA public key")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(pub, digest[:], r, s) {
		return Entitlement{}, errSignature("signature verification failed")
	}

	// —— payload 裁决 ——
	var payload struct {
		BundleID       string `json:"bundleId"`
		ProductID      string `json:"productId"`
		Environment    string `json:"environment"`
		ExpiresDate    *int64 `json:"expiresDate"`
		RevocationDate *int64 `json:"revocationDate"`
	}
	payloadJSON, err := decodeSegment(parts[1])
	if err != nil {
		return Entitlement{}, errInvalid("bad payload encoding")
	}
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		return Entitlement{}, errInvalid("bad payload json")
	}
	if payload.BundleID != v.bundleID {
		return Entitlement{}, errRejected("bundleId mismatch")
	}
	if !v.products[payload.ProductID] {
		return Entitlement{}, errRejected("productId not in pro allowlist")
	}
	env := strings.ToUpper(strings.TrimSpace(payload.Environment))
	if !v.envs[env] {
		return Entitlement{}, errRejected("environment " + env + " not allowed")
	}
	if payload.RevocationDate != nil && *payload.RevocationDate > 0 {
		// 退款撤销:Apple 原生语义,撤销即失效
		return Entitlement{}, errRejected("transaction revoked")
	}
	ent := Entitlement{
		ProductID:   payload.ProductID,
		Environment: env,
	}
	if payload.ExpiresDate != nil && *payload.ExpiresDate > 0 {
		exp := time.UnixMilli(*payload.ExpiresDate)
		if !exp.After(v.now()) {
			return Entitlement{}, errRejected("subscription expired")
		}
		ent.ExpiresDate = &exp
	}
	// ExpiresDate 为空 = 买断(Non-Consumable 无到期概念),白名单已限定产品
	return ent, nil
}

// decodeSegment 解 JWS base64url 段(RawURLEncoding,无 padding)。
func decodeSegment(seg string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(seg)
}

// 错误分类:仅用于日志归因与测试断言,HTTP 对外统一 400 文案。
type errCategory string

const (
	errCatInvalid   errCategory = "malformed"
	errCatChain     errCategory = "chain"
	errCatSignature errCategory = "signature"
	errCatRejected  errCategory = "rejected"
)

type jwsError struct {
	cat errCategory
	msg string
}

func (e jwsError) Error() string { return "proentitlement: " + string(e.cat) + ": " + e.msg }

func errInvalid(msg string) error   { return jwsError{errCatInvalid, msg} }
func errChain(msg string) error     { return jwsError{errCatChain, msg} }
func errSignature(msg string) error { return jwsError{errCatSignature, msg} }
func errRejected(msg string) error  { return jwsError{errCatRejected, msg} }

// Category 供日志/测试归类失败原因。
func Category(err error) errCategory {
	var je jwsError
	if errors.As(err, &je) {
		return je.cat
	}
	return ""
}
