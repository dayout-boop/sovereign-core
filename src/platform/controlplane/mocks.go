package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────
// Mock 어댑터 — 엔진/AWS/Vault/Stripe 없이 "성공" 반환.
// 진짜 교체 시 이 파일만 갈아끼움.
//
// [sovereign_core] T9-A 위조 토큰 결함 수정 (2026-07-03):
//   MockAuth 가 "mock." 접두사만 확인하던 구조를 HMAC-SHA256 서명 검증으로 교체.
//   테스트 환경에서도 실제 서명 검증을 적용하는 것이 2025-2026년 권장 방식
//   (Stripe, Auth0 모두 테스트 환경에서도 실제 JWT 서명 사용).
//   공격자가 "mock." + base64(임의 JSON) 으로 만든 위조 토큰은 서명 불일치로 거부됨.
// ─────────────────────────────────────────────────────────────────────────

func newID(prefix string) string {
	b := make([]byte, 9)
	_, _ = rand.Read(b)
	return prefix + "_" + strings.TrimRight(base64.URLEncoding.EncodeToString(b), "=")
}

// ── StoragePort mock = 가짜 엔진. ──
// coldBootMS = 콜드부팅 시뮬값(웜풀 미스 시 보고). 진짜는 PVM/Firecracker 실측.
type MockStorage struct {
	region     string
	coldBootMS int64
}

func (m *MockStorage) CreateBranch(_ context.Context, _, _, _ string) (string, error) {
	return newID("br"), nil // 진짜: 엔진 CoW 브랜치 (O(1))
}
func (m *MockStorage) DeleteBranch(_ context.Context, _, _ string) error { return nil }

func (m *MockStorage) BootInstance(_ context.Context, region string) (string, string, int64, error) {
	id := newID("inst")
	host := fmt.Sprintf("%s.%s.compute.internal", id, region)
	return id, host, m.coldBootMS, nil // 진짜: 콜드부팅 + 실측 bootMS
}
func (m *MockStorage) AttachBranch(_ context.Context, _, _ string) error { return nil }

func (m *MockStorage) SuspendEndpoint(_ context.Context, _, _ string) error { return nil }
func (m *MockStorage) DeleteEndpoint(_ context.Context, _, _ string) error  { return nil }

// ── AuthPort mock = HMAC-SHA256 서명 기반 토큰 (T9-A 위조 토큰 결함 수정) ──
//
// 토큰 구조: "mock.<base64url(payload)>.<base64url(HMAC-SHA256(payload, secret))>"
// - payload: JSON 직렬화된 Claims
// - secret: 인스턴스 생성 시 랜덤 32바이트 (프로세스 내 유일, 외부 추측 불가)
// - 위조 시도: payload 를 바꾸면 서명 불일치 → 거부. 서명 없으면 파트 수 불일치 → 거부.
type MockAuth struct {
	secret []byte // 랜덤 서명 키 (인스턴스별 고유)
}

func NewMockAuth() *MockAuth {
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	return &MockAuth{secret: secret}
}

func (a *MockAuth) sign(payload []byte) string {
	mac := hmac.New(sha256.New, a.secret)
	mac.Write(payload)
	return base64.URLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *MockAuth) IssueToken(_ context.Context, orgID, userID, role string) (string, error) {
	c := Claims{OrgID: orgID, UserID: userID, Role: role}
	j, _ := json.Marshal(c)
	payload := base64.URLEncoding.EncodeToString(j)
	sig := a.sign(j)
	return "mock." + payload + "." + sig, nil // 진짜: 서명 JWT(JWKS)
}

func (a *MockAuth) VerifyToken(_ context.Context, tok string) (Claims, error) {
	var c Claims
	// 파트 수 검증: 반드시 3파트 (prefix.payload.signature)
	parts := strings.SplitN(tok, ".", 3)
	if len(parts) != 3 || parts[0] != "mock" {
		return c, fmt.Errorf("invalid token: wrong format or prefix")
	}
	// payload 디코딩
	raw, err := base64.URLEncoding.DecodeString(parts[1])
	if err != nil {
		return c, fmt.Errorf("invalid token: payload decode failed")
	}
	// 서명 검증 (HMAC-SHA256, constant-time 비교)
	expectedSig := a.sign(raw)
	if !hmac.Equal([]byte(parts[2]), []byte(expectedSig)) {
		return c, fmt.Errorf("invalid token: signature mismatch")
	}
	// Claims 파싱
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("invalid token: claims parse failed")
	}
	if c.OrgID == "" {
		return c, fmt.Errorf("invalid token: missing org claim")
	}
	return c, nil
}

func (a *MockAuth) JWKS(_ context.Context) (string, error) { return `{"keys":[]}`, nil }

// ── KmsPort mock = 봉투암호화 루트. 평문 미보유(키 참조만). ──
type MockKms struct{}

func (k *MockKms) ProvisionTenantKEK(_ context.Context, _ string) (string, error) {
	return newID("kek"), nil // 진짜: AWS KMS CreateKey + 테넌트 KEK
}

// ── SecretPort mock = DB 자격증명 발급. ──
type MockSecret struct{}

func (s *MockSecret) IssueDBCredential(_ context.Context, _, endpointID string) (string, string, error) {
	pw := make([]byte, 12)
	_, _ = rand.Read(pw)
	return "u_" + endpointID, base64.URLEncoding.EncodeToString(pw), nil // 진짜: Vault
}

// ── CustomerPort mock = 고객 등록 (L0만 구현). ──
// 구독/인보이스/환불은 MultiPGPaymentAdapter 가 담당.
// 진짜 전환: CreateCustomer 를 Stripe/Toss SDK 호출로 교체.
type MockPayment struct{}

func (p *MockPayment) CreateCustomer(_ context.Context, _ string) (string, error) {
	return newID("cus"), nil // 진짜: Stripe/Toss Customer API
}

// 컴파일 타임 인터페이스 준수 검증.
var _ CustomerPort = (*MockPayment)(nil)

// MockPayment PaymentFailurePort 구현 (L4 유예 기간).
func (p *MockPayment) HandlePaymentFailure(_ context.Context, _ string, failedAt time.Time) (time.Time, error) {
	return failedAt.Add(3 * 24 * time.Hour), nil // 기본 3일 유예
}

// 컴파일 타임 인터페이스 준수 검증.
var _ PaymentFailurePort = (*MockPayment)(nil)

// ── MockInvoice — InvoicePort 테스트용 구현. ──
type MockInvoice struct{}

func (m *MockInvoice) CreateInvoice(_ context.Context, orgID string, periodStart, periodEnd time.Time, items []InvoiceLineItem) (*Invoice, error) {
	var total int64
	for _, item := range items {
		total += item.TotalMicro
	}
	return &Invoice{
		ID:          newID("inv"),
		OrgID:       orgID,
		PeriodStart: periodStart,
		PeriodEnd:   periodEnd,
		LineItems:   items,
		TotalMicro:  total,
		Status:      "issued",
		IssuedAt:    time.Now(),
	}, nil
}

func (m *MockInvoice) ChargeInvoice(_ context.Context, _, _ string) error {
	return nil
}

// 컴파일 타임 인터페이스 준수 검증.
var _ InvoicePort = (*MockInvoice)(nil)
