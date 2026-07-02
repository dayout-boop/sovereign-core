package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────
// Mock 어댑터 — 엔진/AWS/Vault/Stripe 없이 "성공" 반환.
// 진짜 교체 시 이 파일만 갈아끼움.
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

// ── AuthPort mock = 가짜 OIDC IdP. JWT 대신 서명 없는 토큰(구조만). ──
type MockAuth struct{}

func (a *MockAuth) IssueToken(_ context.Context, orgID, userID, role string) (string, error) {
	c := Claims{OrgID: orgID, UserID: userID, Role: role}
	j, _ := json.Marshal(c)
	return "mock." + base64.URLEncoding.EncodeToString(j), nil // 진짜: 서명 JWT(JWKS)
}
func (a *MockAuth) VerifyToken(_ context.Context, tok string) (Claims, error) {
	var c Claims
	parts := strings.SplitN(tok, ".", 2)
	if len(parts) != 2 || parts[0] != "mock" {
		return c, fmt.Errorf("invalid token")
	}
	raw, err := base64.URLEncoding.DecodeString(parts[1])
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, err
	}
	if c.OrgID == "" {
		return c, fmt.Errorf("missing org claim")
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

// ── PaymentPort mock = platform 결제 고객 생성. ──
type MockPayment struct{}

func (p *MockPayment) CreateCustomer(_ context.Context, _ string) (string, error) {
	return newID("cus"), nil // 진짜: Stripe Customer
}
