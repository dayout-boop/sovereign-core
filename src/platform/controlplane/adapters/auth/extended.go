package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────
// adapter_auth_extended.go — 확장 인증 포트 인메모리 구현체
//
// 포함:
//   - OAuth2PKCEAdapter: OAuth 2.0 + PKCE 인가 흐름
//   - DeviceFlowAdapter: CLI/헤드리스 Device Authorization Flow
//   - APIKeyAdapter: API Key SHA-256 해시 저장 + HMAC-SHA256 서명
// ─────────────────────────────────────────────────────────────────────────

// ─── OAuth2PKCEAdapter ────────────────────────────────────────────────────

type pkceSession struct {
	State       string
	ClientID    string
	RedirectURI string
	Challenge   PKCEChallenge
	Code        string
	ExpiresAt   time.Time
}

// OAuth2PKCEAdapter — OAuth2PKCEPort 인메모리 구현체.
type OAuth2PKCEAdapter struct {
	mu       sync.RWMutex
	sessions map[string]*pkceSession // state → session
	tokens   map[string]*OAuth2Token // refreshToken → token
}

// 컴파일 타임 인터페이스 계약 검증.
var _ OAuth2PKCEPort = (*OAuth2PKCEAdapter)(nil)

// NewOAuth2PKCEAdapter — OAuth2PKCEAdapter 생성.
func NewOAuth2PKCEAdapter() *OAuth2PKCEAdapter {
	return &OAuth2PKCEAdapter{
		sessions: make(map[string]*pkceSession),
		tokens:   make(map[string]*OAuth2Token),
	}
}

func (a *OAuth2PKCEAdapter) AuthorizeURL(ctx context.Context, clientID, redirectURI, scope string, challenge PKCEChallenge) (string, string, error) {
	if challenge.Method != "S256" {
		return "", "", fmt.Errorf("pkce: only S256 method allowed, got %q", challenge.Method)
	}
	state := newID("state")
	code := newID("code")
	a.mu.Lock()
	a.sessions[state] = &pkceSession{
		State:       state,
		ClientID:    clientID,
		RedirectURI: redirectURI,
		Challenge:   challenge,
		Code:        code,
		ExpiresAt:   time.Now().Add(10 * time.Minute),
	}
	a.mu.Unlock()
	authURL := fmt.Sprintf("https://auth.internal/oauth2/authorize?response_type=code&client_id=%s&state=%s&code_challenge=%s&code_challenge_method=S256", clientID, state, challenge.CodeChallenge)
	return authURL, state, nil
}

func (a *OAuth2PKCEAdapter) ExchangeCode(ctx context.Context, code, codeVerifier, redirectURI string) (*OAuth2Token, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, sess := range a.sessions {
		if sess.Code != code {
			continue
		}
		if time.Now().After(sess.ExpiresAt) {
			return nil, fmt.Errorf("pkce: authorization code expired")
		}
		// PKCE 검증: SHA-256(codeVerifier) == codeChallenge.
		h := sha256.Sum256([]byte(codeVerifier))
		computed := hex.EncodeToString(h[:])
		if computed != sess.Challenge.CodeChallenge {
			return nil, fmt.Errorf("pkce: code verifier mismatch")
		}
		token := &OAuth2Token{
			AccessToken:  newID("at"),
			TokenType:    "Bearer",
			ExpiresAt:    time.Now().Add(15 * time.Minute),
			RefreshToken: newID("rt"),
			Scope:        "openid profile",
		}
		a.tokens[token.RefreshToken] = token
		delete(a.sessions, sess.State)
		return token, nil
	}
	return nil, fmt.Errorf("pkce: authorization code not found")
}

func (a *OAuth2PKCEAdapter) RefreshToken(ctx context.Context, refreshToken string) (*OAuth2Token, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	old, ok := a.tokens[refreshToken]
	if !ok {
		return nil, fmt.Errorf("pkce: refresh token not found")
	}
	newToken := &OAuth2Token{
		AccessToken:  newID("at"),
		TokenType:    "Bearer",
		ExpiresAt:    time.Now().Add(15 * time.Minute),
		RefreshToken: newID("rt"),
		Scope:        old.Scope,
	}
	delete(a.tokens, refreshToken)
	a.tokens[newToken.RefreshToken] = newToken
	return newToken, nil
}

func (a *OAuth2PKCEAdapter) RevokeToken(ctx context.Context, token string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.tokens, token)
	return nil
}

// ─── DeviceFlowAdapter ────────────────────────────────────────────────────

type deviceSession struct {
	DeviceCode string
	UserCode   string
	ClientID   string
	Authorized bool
	ExpiresAt  time.Time
}

// DeviceFlowAdapter — DeviceFlowPort 인메모리 구현체.
type DeviceFlowAdapter struct {
	mu       sync.RWMutex
	sessions map[string]*deviceSession // deviceCode → session
}

// 컴파일 타임 인터페이스 계약 검증.
var _ DeviceFlowPort = (*DeviceFlowAdapter)(nil)

// NewDeviceFlowAdapter — DeviceFlowAdapter 생성.
func NewDeviceFlowAdapter() *DeviceFlowAdapter {
	return &DeviceFlowAdapter{sessions: make(map[string]*deviceSession)}
}

func (a *DeviceFlowAdapter) RequestDeviceCode(ctx context.Context, clientID, scope string) (*DeviceCode, error) {
	dc := newID("dc")
	uc := newID("uc")[:8] // 짧은 사용자 코드
	sess := &deviceSession{
		DeviceCode: dc,
		UserCode:   uc,
		ClientID:   clientID,
		ExpiresAt:  time.Now().Add(15 * time.Minute),
	}
	a.mu.Lock()
	a.sessions[dc] = sess
	a.mu.Unlock()
	return &DeviceCode{
		DeviceCode:      dc,
		UserCode:        uc,
		VerificationURI: "https://auth.internal/device",
		ExpiresIn:       15 * time.Minute,
		Interval:        5 * time.Second,
	}, nil
}

func (a *DeviceFlowAdapter) PollToken(ctx context.Context, deviceCode string) (*OAuth2Token, error) {
	a.mu.RLock()
	sess, ok := a.sessions[deviceCode]
	a.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("device_flow: device code not found")
	}
	if time.Now().After(sess.ExpiresAt) {
		return nil, fmt.Errorf("device_flow: device code expired")
	}
	if !sess.Authorized {
		return nil, nil // 아직 미승인 — 클라이언트는 재폴링.
	}
	return &OAuth2Token{
		AccessToken:  newID("at"),
		TokenType:    "Bearer",
		ExpiresAt:    time.Now().Add(15 * time.Minute),
		RefreshToken: newID("rt"),
	}, nil
}

// AuthorizeDeviceCode — 테스트/어드민용: 특정 UserCode를 승인 처리.
func (a *DeviceFlowAdapter) AuthorizeDeviceCode(deviceCode string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if sess, ok := a.sessions[deviceCode]; ok {
		sess.Authorized = true
	}
}

// ─── APIKeyAdapter ────────────────────────────────────────────────────────

type apiKeyRecord struct {
	Info      APIKeyInfo
	HashHex   string // SHA-256(plainKey) hex
	SecretKey []byte // HMAC 서명 검증용 비밀키 (평문 저장 금지 — 실제 구현에서는 KMS 위임)
}

// APIKeyAdapter — APIKeyPort 인메모리 구현체.
type APIKeyAdapter struct {
	mu      sync.RWMutex
	keys    map[string]*apiKeyRecord // keyID → record
	hashIdx map[string]string        // hashHex → keyID (빠른 역조회)
}

// 컴파일 타임 인터페이스 계약 검증.
var _ APIKeyPort = (*APIKeyAdapter)(nil)

// NewAPIKeyAdapter — APIKeyAdapter 생성.
func NewAPIKeyAdapter() *APIKeyAdapter {
	return &APIKeyAdapter{
		keys:    make(map[string]*apiKeyRecord),
		hashIdx: make(map[string]string),
	}
}

func (a *APIKeyAdapter) IssueAPIKey(ctx context.Context, orgID, name string, scopes []string, expiresAt time.Time) (string, *APIKeyInfo, error) {
	plainKey := newID("sk") + newID("") // 충분히 긴 랜덤 키
	h := sha256.Sum256([]byte(plainKey))
	hashHex := hex.EncodeToString(h[:])

	keyID := newID("kid")
	info := APIKeyInfo{
		KeyID:     keyID,
		OrgID:     orgID,
		Name:      name,
		Scopes:    scopes,
		ExpiresAt: expiresAt,
		CreatedAt: time.Now(),
	}
	rec := &apiKeyRecord{
		Info:      info,
		HashHex:   hashHex,
		SecretKey: []byte(plainKey), // 실제: KMS에서 별도 서명 키 발급
	}

	a.mu.Lock()
	a.keys[keyID] = rec
	a.hashIdx[hashHex] = keyID
	a.mu.Unlock()

	return plainKey, &info, nil
}

func (a *APIKeyAdapter) VerifyAPIKey(ctx context.Context, plainKey string) (*APIKeyInfo, error) {
	h := sha256.Sum256([]byte(plainKey))
	hashHex := hex.EncodeToString(h[:])

	a.mu.RLock()
	defer a.mu.RUnlock()

	keyID, ok := a.hashIdx[hashHex]
	if !ok {
		return nil, fmt.Errorf("api_key: invalid key")
	}
	rec := a.keys[keyID]
	if !rec.Info.ExpiresAt.IsZero() && time.Now().After(rec.Info.ExpiresAt) {
		return nil, fmt.Errorf("api_key: key expired")
	}
	info := rec.Info
	info.LastUsedAt = time.Now()
	return &info, nil
}

func (a *APIKeyAdapter) VerifyHMAC(ctx context.Context, keyID string, payload []byte, timestamp time.Time, signature string) error {
	// 타임스탬프 ±5분 초과 시 재생 공격으로 간주하여 거부.
	diff := time.Since(timestamp)
	if diff < 0 {
		diff = -diff
	}
	if diff > 5*time.Minute {
		return fmt.Errorf("api_key: timestamp out of range (replay attack prevention)")
	}

	a.mu.RLock()
	rec, ok := a.keys[keyID]
	a.mu.RUnlock()
	if !ok {
		return fmt.Errorf("api_key: key not found: %s", keyID)
	}

	mac := hmac.New(sha256.New, rec.SecretKey)
	mac.Write(payload)
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(signature)) {
		return fmt.Errorf("api_key: HMAC signature mismatch")
	}
	return nil
}

func (a *APIKeyAdapter) RevokeAPIKey(ctx context.Context, keyID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	rec, ok := a.keys[keyID]
	if !ok {
		return fmt.Errorf("api_key: key not found: %s", keyID)
	}
	delete(a.hashIdx, rec.HashHex)
	delete(a.keys, keyID)
	return nil
}

func (a *APIKeyAdapter) ListAPIKeys(ctx context.Context, orgID string) ([]APIKeyInfo, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	var result []APIKeyInfo
	for _, rec := range a.keys {
		if rec.Info.OrgID == orgID {
			result = append(result, rec.Info)
		}
	}
	return result, nil
}

// signForTest — 테스트 전용 HMAC 서명 생성 헬퍼.
func (a *APIKeyAdapter) signForTest(keyID string, payload []byte) string {
	a.mu.RLock()
	rec, ok := a.keys[keyID]
	a.mu.RUnlock()
	if !ok {
		return ""
	}
	mac := hmac.New(sha256.New, rec.SecretKey)
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}
