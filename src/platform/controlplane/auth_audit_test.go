package main

// auth_audit_test.go
// OAuth2PKCE / DeviceFlow / APIKey 예외·경계값·동시성·부분 실패 케이스 전수 보완

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// ──────────────────────────────────────────────────────────────────────────────
// OAuth2 PKCE 누락 케이스
// ──────────────────────────────────────────────────────────────────────────────

// 1. S256 이외의 PKCE 메서드 → 에러 (plain 메서드 금지)
func TestPKCE_PlainMethod_Rejected(t *testing.T) {
	a := NewOAuth2PKCEAdapter()
	_, _, err := a.AuthorizeURL(context.Background(), "client_1", "https://app/callback", "read",
		PKCEChallenge{Method: "plain", CodeChallenge: "abc123"})
	if err == nil {
		t.Fatal("expected error for plain PKCE method")
	}
	if !strings.Contains(err.Error(), "S256") {
		t.Fatalf("error should mention S256 requirement, got: %v", err)
	}
}

// 2. 만료된 인가 코드 교환 → 에러
func TestPKCE_ExpiredCode_Error(t *testing.T) {
	a := NewOAuth2PKCEAdapter()
	// 세션 직접 삽입 (1초 만료)
	code := "code_expired_test"
	a.mu.Lock()
	a.sessions["state_exp"] = &pkceSession{
		State: "state_exp", Code: code, ClientID: "client_1",
		RedirectURI: "https://app/callback",
		Challenge:   PKCEChallenge{Method: "S256", CodeChallenge: "dummy", CodeVerifier: "verifier_abc"},
		ExpiresAt:   time.Now().Add(1 * time.Second),
	}
	a.mu.Unlock()
	time.Sleep(2 * time.Second)
	_, err := a.ExchangeCode(context.Background(), code, "verifier_abc", "https://app/callback")
	if err == nil {
		t.Fatal("expected error for expired authorization code")
	}
}

// 3. 잘못된 코드 검증자(verifier) → 에러
func TestPKCE_WrongVerifier_Error(t *testing.T) {
	a := NewOAuth2PKCEAdapter()
	// 올바른 verifier의 S256 해시로 challenge 설정
	h := sha256.Sum256([]byte("correct_verifier"))
	codeChallenge := hex.EncodeToString(h[:])
	code := "code_wrong_verifier"
	a.mu.Lock()
	a.sessions["state_wv"] = &pkceSession{
		State: "state_wv", Code: code, ClientID: "client_1",
		RedirectURI: "https://app/callback",
		Challenge:   PKCEChallenge{Method: "S256", CodeChallenge: codeChallenge, CodeVerifier: "correct_verifier"},
		ExpiresAt:   time.Now().Add(5 * time.Minute),
	}
	a.mu.Unlock()
	_, err := a.ExchangeCode(context.Background(), code, "wrong_verifier", "https://app/callback")
	if err == nil {
		t.Fatal("expected error for wrong code verifier")
	}
}

// 4. 존재하지 않는 인가 코드 교환 → 에러
func TestPKCE_NonExistentCode_Error(t *testing.T) {
	a := NewOAuth2PKCEAdapter()
	_, err := a.ExchangeCode(context.Background(), "nonexistent_code", "verifier", "https://app/callback")
	if err == nil {
		t.Fatal("expected error for non-existent authorization code")
	}
}

// 5. 존재하지 않는 Refresh Token → 에러
func TestPKCE_InvalidRefreshToken_Error(t *testing.T) {
	a := NewOAuth2PKCEAdapter()
	_, err := a.RefreshToken(context.Background(), "invalid_refresh_token_xyz")
	if err == nil {
		t.Fatal("expected error for invalid refresh token")
	}
}

// 6. 폐기된 토큰으로 Refresh 시도 → 에러
func TestPKCE_RevokedToken_Refresh_Error(t *testing.T) {
	a := NewOAuth2PKCEAdapter()
	h := sha256.Sum256([]byte("verifier_rev"))
	codeChallenge := hex.EncodeToString(h[:])
	code := "code_rev_test"
	a.mu.Lock()
	a.sessions["state_rev"] = &pkceSession{
		State: "state_rev", Code: code, ClientID: "client_1",
		RedirectURI: "https://app/callback",
		Challenge:   PKCEChallenge{Method: "S256", CodeChallenge: codeChallenge, CodeVerifier: "verifier_rev"},
		ExpiresAt:   time.Now().Add(5 * time.Minute),
	}
	a.mu.Unlock()
	token, err := a.ExchangeCode(context.Background(), code, "verifier_rev", "https://app/callback")
	if err != nil {
		t.Fatalf("exchange failed: %v", err)
	}
	// 토큰 폐기
	if err := a.RevokeToken(context.Background(), token.AccessToken); err != nil {
		t.Fatalf("revoke failed: %v", err)
	}
	// 폐기된 refresh token으로 재발급 시도
	_, err = a.RefreshToken(context.Background(), token.RefreshToken)
	if err == nil {
		t.Fatal("expected error for revoked refresh token")
	}
}

// 7. 동시 코드 교환 — 동일 코드 재사용 방지 (코드 1회 사용 원칙)
func TestPKCE_CodeReuse_OnlyFirstSucceeds(t *testing.T) {
	a := NewOAuth2PKCEAdapter()
	h := sha256.Sum256([]byte("verifier_once"))
	codeChallenge := hex.EncodeToString(h[:])
	code := "code_once_test"
	a.mu.Lock()
	a.sessions["state_once"] = &pkceSession{
		State: "state_once", Code: code, ClientID: "client_1",
		RedirectURI: "https://app/callback",
		Challenge:   PKCEChallenge{Method: "S256", CodeChallenge: codeChallenge, CodeVerifier: "verifier_once"},
		ExpiresAt:   time.Now().Add(5 * time.Minute),
	}
	a.mu.Unlock()
	code = "code_once_test"

	var wg sync.WaitGroup
	successCount := 0
	var mu sync.Mutex
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.ExchangeCode(context.Background(), code, "verifier_once", "https://app/callback"); err == nil {
				mu.Lock()
				successCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if successCount != 1 {
		t.Fatalf("authorization code must be single-use, got %d successes", successCount)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// DeviceFlow 누락 케이스
// ──────────────────────────────────────────────────────────────────────────────

// 8. 빈 clientID → 에러
func TestDeviceFlow_EmptyClientID_Error(t *testing.T) {
	a := NewDeviceFlowAdapter()
	_, err := a.RequestDeviceCode(context.Background(), "", "read")
	if err == nil {
		t.Fatal("expected error for empty clientID")
	}
}

// 9. 만료된 device code 폴링 → 에러
func TestDeviceFlow_ExpiredCode_Error(t *testing.T) {
	a := NewDeviceFlowAdapter()
	dc, err := a.RequestDeviceCode(context.Background(), "cli_client", "read")
	if err != nil {
		t.Fatalf("request device code failed: %v", err)
	}
	// 만료 강제 설정
	a.mu.Lock()
	if rec, ok := a.sessions[dc.DeviceCode]; ok {
		rec.ExpiresAt = time.Now().Add(-1 * time.Second)
		a.sessions[dc.DeviceCode] = rec
	}
	a.mu.Unlock()
	_, err = a.PollToken(context.Background(), dc.DeviceCode)
	if err == nil {
		t.Fatal("expected error for expired device code")
	}
}

// 10. 미승인 상태 폴링 → nil 반환 (에러 아님, 클라이언트 재폴링 신호)
func TestDeviceFlow_PendingApproval_ReturnsNil(t *testing.T) {
	a := NewDeviceFlowAdapter()
	dc, err := a.RequestDeviceCode(context.Background(), "cli_client", "read")
	if err != nil {
		t.Fatalf("request device code failed: %v", err)
	}
	token, err := a.PollToken(context.Background(), dc.DeviceCode)
	if err != nil {
		t.Fatalf("pending poll should not error, got: %v", err)
	}
	if token != nil {
		t.Fatal("pending poll should return nil token")
	}
}

// 11. 승인 후 폴링 → 토큰 반환
func TestDeviceFlow_ApprovedPoll_ReturnsToken(t *testing.T) {
	a := NewDeviceFlowAdapter()
	dc, err := a.RequestDeviceCode(context.Background(), "cli_client", "read")
	if err != nil {
		t.Fatalf("request device code failed: %v", err)
	}
	// 사용자 승인 시뮬레이션
	a.AuthorizeDeviceCode(dc.DeviceCode)
	token, err := a.PollToken(context.Background(), dc.DeviceCode)
	if err != nil {
		t.Fatalf("approved poll failed: %v", err)
	}
	if token == nil {
		t.Fatal("approved poll should return token")
	}
}

// 12. 존재하지 않는 device code 폴링 → 에러
func TestDeviceFlow_NonExistentCode_Error(t *testing.T) {
	a := NewDeviceFlowAdapter()
	_, err := a.PollToken(context.Background(), "nonexistent_device_code")
	if err == nil {
		t.Fatal("expected error for non-existent device code")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// APIKey 누락 케이스
// ──────────────────────────────────────────────────────────────────────────────

// 13. 빈 orgID API Key 발급 → 에러
func TestAPIKey_EmptyOrgID_Error(t *testing.T) {
	a := NewAPIKeyAdapter()
	_, _, err := a.IssueAPIKey(context.Background(), "", "test-key", []string{"read"}, time.Now().Add(24*time.Hour))
	if err == nil {
		t.Fatal("expected error for empty orgID")
	}
}

// 14. 과거 만료 시간 API Key 발급 → 에러
func TestAPIKey_PastExpiry_Error(t *testing.T) {
	a := NewAPIKeyAdapter()
	_, _, err := a.IssueAPIKey(context.Background(), "org_1", "test-key", []string{"read"}, time.Now().Add(-1*time.Hour))
	if err == nil {
		t.Fatal("expected error for past expiry time")
	}
}

// 15. 만료된 API Key 검증 → 에러
func TestAPIKey_ExpiredKey_VerifyError(t *testing.T) {
	a := NewAPIKeyAdapter()
	// 1초 후 만료되는 키 발급
	plain, _, err := a.IssueAPIKey(context.Background(), "org_1", "expiring-key", []string{"read"}, time.Now().Add(1*time.Second))
	if err != nil {
		t.Fatalf("issue failed: %v", err)
	}
	time.Sleep(2 * time.Second)
	_, err = a.VerifyAPIKey(context.Background(), plain)
	if err == nil {
		t.Fatal("expected error for expired API key")
	}
}

// 16. 폐기된 API Key 검증 → 에러
func TestAPIKey_RevokedKey_VerifyError(t *testing.T) {
	a := NewAPIKeyAdapter()
	plain, info, err := a.IssueAPIKey(context.Background(), "org_1", "revoke-key", []string{"read"}, time.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatalf("issue failed: %v", err)
	}
	if err := a.RevokeAPIKey(context.Background(), info.KeyID); err != nil {
		t.Fatalf("revoke failed: %v", err)
	}
	_, err = a.VerifyAPIKey(context.Background(), plain)
	if err == nil {
		t.Fatal("expected error for revoked API key")
	}
}

// 17. HMAC 서명 — 5분 초과 타임스탬프 → 재생 공격 방지
func TestAPIKey_HMAC_OldTimestamp_ReplayBlocked(t *testing.T) {
	a := NewAPIKeyAdapter()
	_, info, err := a.IssueAPIKey(context.Background(), "org_1", "hmac-key", []string{"write"}, time.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatalf("issue failed: %v", err)
	}
	payload := []byte(`{"action":"charge","amount":1000}`)
	oldTimestamp := time.Now().Add(-6 * time.Minute) // 5분 초과
	sig := a.signForTest(info.KeyID, payload)
	err = a.VerifyHMAC(context.Background(), info.KeyID, payload, oldTimestamp, sig)
	if err == nil {
		t.Fatal("expected error for old timestamp (replay attack)")
	}
}

// 18. HMAC 서명 — 미래 타임스탬프 → 재생 공격 방지
func TestAPIKey_HMAC_FutureTimestamp_ReplayBlocked(t *testing.T) {
	a := NewAPIKeyAdapter()
	_, info, err := a.IssueAPIKey(context.Background(), "org_1", "hmac-key2", []string{"write"}, time.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatalf("issue failed: %v", err)
	}
	payload := []byte(`{"action":"charge","amount":2000}`)
	futureTimestamp := time.Now().Add(6 * time.Minute) // 미래 5분 초과
	sig := a.signForTest(info.KeyID, payload)
	err = a.VerifyHMAC(context.Background(), info.KeyID, payload, futureTimestamp, sig)
	if err == nil {
		t.Fatal("expected error for future timestamp (replay attack)")
	}
}

// 19. HMAC 서명 — 페이로드 변조 → 서명 불일치
func TestAPIKey_HMAC_TamperedPayload_Error(t *testing.T) {
	a := NewAPIKeyAdapter()
	_, info, err := a.IssueAPIKey(context.Background(), "org_1", "hmac-key3", []string{"write"}, time.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatalf("issue failed: %v", err)
	}
	originalPayload := []byte(`{"action":"charge","amount":1000}`)
	tamperedPayload := []byte(`{"action":"charge","amount":9999}`) // 변조
	ts := time.Now()
	sig := a.signForTest(info.KeyID, originalPayload)
	err = a.VerifyHMAC(context.Background(), info.KeyID, tamperedPayload, ts, sig)
	if err == nil {
		t.Fatal("expected error for tampered payload")
	}
}

// 20. 존재하지 않는 keyID HMAC 검증 → 에러
func TestAPIKey_HMAC_NonExistentKey_Error(t *testing.T) {
	a := NewAPIKeyAdapter()
	err := a.VerifyHMAC(context.Background(), "nonexistent_key_id", []byte("payload"), time.Now(), "sig")
	if err == nil {
		t.Fatal("expected error for non-existent keyID")
	}
}

// 21. ListAPIKeys — 폐기된 키 포함 여부 확인
func TestAPIKey_ListKeys_IncludesRevoked(t *testing.T) {
	a := NewAPIKeyAdapter()
	_, info1, _ := a.IssueAPIKey(context.Background(), "org_list", "key-active", []string{"read"}, time.Now().Add(24*time.Hour))
	_, info2, _ := a.IssueAPIKey(context.Background(), "org_list", "key-revoked", []string{"read"}, time.Now().Add(24*time.Hour))
	_ = a.RevokeAPIKey(context.Background(), info2.KeyID)

	keys, err := a.ListAPIKeys(context.Background(), "org_list")
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if len(keys) < 2 {
		t.Fatalf("expected at least 2 keys (including revoked), got %d", len(keys))
	}
	_ = info1
}

// 22. 동시 IssueAPIKey — race detector 통과
func TestAPIKey_ConcurrentIssue_NoRace(t *testing.T) {
	a := NewAPIKeyAdapter()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, _ = a.IssueAPIKey(context.Background(),
				fmt.Sprintf("org_%d", i),
				fmt.Sprintf("key_%d", i),
				[]string{"read"},
				time.Now().Add(24*time.Hour))
		}(i)
	}
	wg.Wait()
}

// 23. 동시 VerifyAPIKey — race detector 통과
func TestAPIKey_ConcurrentVerify_NoRace(t *testing.T) {
	a := NewAPIKeyAdapter()
	plain, _, err := a.IssueAPIKey(context.Background(), "org_concurrent", "shared-key", []string{"read"}, time.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatalf("issue failed: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = a.VerifyAPIKey(context.Background(), plain)
		}()
	}
	wg.Wait()
}
