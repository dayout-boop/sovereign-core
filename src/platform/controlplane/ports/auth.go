package main

import (
	"context"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────
// auth_ports.go — 환경별 인증 포트 정의
//
// 설계 결정 (PAYMENT_AUTH_DESIGN_DECISIONS.md §4, §5 기반):
//   - 웹/모바일/데스크탑: OAuth 2.0 + PKCE (Authorization Code Flow)
//   - CLI/헤드리스: OAuth 2.0 Device Authorization Flow
//   - 내부 서비스 간: mTLS + 단기 JWT (15분)
//   - API Key: SHA-256 해시 저장, HMAC-SHA256 요청 서명
// ─────────────────────────────────────────────────────────────────────────

// ─── OAuth 2.0 + PKCE 포트 ───────────────────────────────────────────────

// PKCEChallenge — PKCE 코드 챌린지 정보.
// 모바일/데스크탑 앱에서 Authorization Code 가로채기 공격 방지.
type PKCEChallenge struct {
	CodeVerifier  string // 클라이언트가 생성한 랜덤 문자열 (43~128자)
	CodeChallenge string // SHA-256(CodeVerifier) → Base64URL 인코딩
	Method        string // "S256" (SHA-256만 허용, plain 금지)
}

// OAuth2Token — OAuth 2.0 토큰 응답.
type OAuth2Token struct {
	AccessToken  string    `json:"access_token"`
	TokenType    string    `json:"token_type"`    // "Bearer"
	ExpiresAt    time.Time `json:"expires_at"`    // 15분
	RefreshToken string    `json:"refresh_token"` // 30일
	Scope        string    `json:"scope"`
}

// OAuth2PKCEPort — OAuth 2.0 + PKCE 인증 흐름 경계.
// 설계 원칙:
//   - 모바일(iOS/Android), 데스크탑 앱에서 필수 사용.
//   - plain 챌린지 메서드 금지, S256만 허용.
//   - Access Token 만료: 15분. Refresh Token 만료: 30일.
type OAuth2PKCEPort interface {
	// AuthorizeURL — PKCE 코드 챌린지를 포함한 인가 URL 생성.
	AuthorizeURL(ctx context.Context, clientID, redirectURI, scope string, challenge PKCEChallenge) (authURL, state string, err error)
	// ExchangeCode — 인가 코드 + 코드 검증자로 토큰 교환.
	ExchangeCode(ctx context.Context, code, codeVerifier, redirectURI string) (*OAuth2Token, error)
	// RefreshToken — Refresh Token으로 새 Access Token 발급.
	RefreshToken(ctx context.Context, refreshToken string) (*OAuth2Token, error)
	// RevokeToken — 토큰 폐기 (로그아웃).
	RevokeToken(ctx context.Context, token string) error
}

// ─── Device Authorization Flow 포트 ────────────────────────────────────

// DeviceCode — Device Authorization Flow 응답.
type DeviceCode struct {
	DeviceCode      string        `json:"device_code"`
	UserCode        string        `json:"user_code"`        // 사용자가 브라우저에 입력하는 코드
	VerificationURI string        `json:"verification_uri"` // 사용자가 방문하는 URL
	ExpiresIn       time.Duration `json:"expires_in"`       // 코드 만료 시간 (기본 15분)
	Interval        time.Duration `json:"interval"`         // 폴링 간격 (기본 5초)
}

// DeviceFlowPort — OAuth 2.0 Device Authorization Flow 경계.
// 설계 원칙:
//   - CLI 도구, 헤드리스 서버, TV 앱 등 브라우저 없는 환경에서 사용.
//   - 사용자는 별도 기기에서 VerificationURI에 접속하여 UserCode 입력.
//   - 클라이언트는 Interval마다 PollToken을 호출하여 토큰 발급 대기.
type DeviceFlowPort interface {
	// RequestDeviceCode — 디바이스 코드 발급 요청.
	RequestDeviceCode(ctx context.Context, clientID, scope string) (*DeviceCode, error)
	// PollToken — 사용자 인증 완료 여부 폴링. 미완료 시 nil 반환.
	PollToken(ctx context.Context, deviceCode string) (*OAuth2Token, error)
}

// ─── mTLS 내부 서비스 인증 포트 ─────────────────────────────────────────

// MTLSIdentity — mTLS 인증서에서 추출한 서비스 신원.
type MTLSIdentity struct {
	ServiceName string    // 서비스 이름 (CN 필드)
	OrgID       string    // 테넌트 ID (SAN 확장 필드)
	IssuedAt    time.Time // 인증서 발급 시각
	ExpiresAt   time.Time // 인증서 만료 시각
}

// MTLSPort — 내부 서비스 간 mTLS 상호 인증 경계.
// 설계 원칙:
//   - Edge ↔ Core 간, 마이크로서비스 간 통신에 사용.
//   - 클라이언트와 서버 모두 인증서를 제시하여 상호 인증.
//   - 인증서 유효 기간: 24시간 (자동 갱신).
type MTLSPort interface {
	// VerifyClientCert — 클라이언트 인증서 검증 및 신원 추출.
	VerifyClientCert(ctx context.Context, certPEM []byte) (*MTLSIdentity, error)
	// IssueServiceCert — 내부 서비스용 단기 인증서 발급 (24시간).
	IssueServiceCert(ctx context.Context, serviceName, orgID string) (certPEM, keyPEM []byte, err error)
	// RevokeServiceCert — 서비스 인증서 폐기 (침해 사고 대응).
	RevokeServiceCert(ctx context.Context, certPEM []byte) error
}

// ─── API Key 인증 포트 ───────────────────────────────────────────────────

// APIKeyInfo — API Key 메타데이터.
type APIKeyInfo struct {
	KeyID       string    `json:"key_id"`
	OrgID       string    `json:"org_id"`
	Name        string    `json:"name"`        // 사용자 지정 이름
	Scopes      []string  `json:"scopes"`      // 허용된 API 범위
	LastUsedAt  time.Time `json:"last_used_at,omitempty"`
	ExpiresAt   time.Time `json:"expires_at,omitempty"` // zero=무기한
	CreatedAt   time.Time `json:"created_at"`
}

// APIKeyPort — API Key 발급 및 검증 경계.
// 설계 원칙:
//   - API Key는 DB에 SHA-256 해시로만 저장. 평문은 발급 시 1회만 노출.
//   - 요청 서명: HMAC-SHA256(payload + timestamp). 타임스탬프 ±5분 초과 시 거부.
//   - 서버-서버 M2M 통신에 사용. 사용자 대리 접근에는 OAuth2+PKCE 사용.
type APIKeyPort interface {
	// IssueAPIKey — 신규 API Key 발급. 평문 키는 이 응답에서만 노출.
	IssueAPIKey(ctx context.Context, orgID, name string, scopes []string, expiresAt time.Time) (plainKey string, info *APIKeyInfo, err error)
	// VerifyAPIKey — API Key 검증. SHA-256 해시로 DB 조회.
	VerifyAPIKey(ctx context.Context, plainKey string) (*APIKeyInfo, error)
	// VerifyHMAC — HMAC-SHA256 요청 서명 검증.
	// payload: 요청 본문 바이트. timestamp: 요청 타임스탬프 (±5분 허용).
	VerifyHMAC(ctx context.Context, keyID string, payload []byte, timestamp time.Time, signature string) error
	// RevokeAPIKey — API Key 폐기.
	RevokeAPIKey(ctx context.Context, keyID string) error
	// ListAPIKeys — 테넌트의 API Key 목록 조회.
	ListAPIKeys(ctx context.Context, orgID string) ([]APIKeyInfo, error)
}
