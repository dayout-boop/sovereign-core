package main

// ─────────────────────────────────────────────────────────────────────────
// T5 — 온보딩 부분커밋(Partial-Commit) 경로 검증.
//
// T4 는 store.put(org) '이전' 실패(KMS/billing)만 봤다. 이 파일은 그 '이후'
// 단계(IssueToken)가 실패할 때 org/membership 이 좀비로 남는지 관측한다.
//
// Signup 순서(app.go): KEK → org객체 → billing → store.put(org)
//                      → addMembership → IssueToken
// 따라서 IssueToken 실패 시:
//   - org 는 이미 store 에 있음(부분커밋 후보)
//   - membership 도 이미 추가됨(부분커밋 후보)
//   - 그런데 Signup 은 에러 반환 → 호출자는 "실패"로 인지
//   = 소유자가 로그인 토큰을 못 받은 "좀비 테넌트"가 남을 수 있다.
//
// 이 테스트는 코드를 고치지 않는다. 결함 유무를 숫자로 드러낼 뿐이다.
// (원자적 온보딩이 이미 보장된다면 org/membership 은 0이어야 한다.)
// ─────────────────────────────────────────────────────────────────────────

import (
	"context"
	"fmt"
	"testing"
)

// 토큰 발급만 실패시키는 mock. 나머지(Verify/JWKS)는 MockAuth 승계.
// MockAuth 의 메서드가 포인터 리시버이므로 *MockAuth 를 임베딩한다.
type failingAuth struct{ *MockAuth }

func (failingAuth) IssueToken(_ context.Context, _, _, _ string) (string, error) {
	return "", fmt.Errorf("idp token endpoint down")
}

func countMemberships(app *App) int {
	app.store.mu.RLock()
	defer app.store.mu.RUnlock()
	return len(app.store.memberships)
}

func TestStage2_T5_TokenFailurePartialCommit(t *testing.T) {
	app := NewApp()
	app.auth = failingAuth{MockAuth: &MockAuth{}}

	orgsBefore := countOrgs(app)
	memBefore := countMemberships(app)

	_, _, err := app.Signup(context.Background(), "tokfail", "owner1")

	orgsAfter := countOrgs(app)
	memAfter := countMemberships(app)

	t.Logf("[T5] err=%v | orgs %d→%d | memberships %d→%d",
		err, orgsBefore, orgsAfter, memBefore, memAfter)

	if err == nil {
		t.Fatalf("[T5] IdP 다운인데 Signup 이 성공 반환(테스트 전제 붕괴)")
	}

	// 단언: 원자적 온보딩이면 실패 시 org/membership 잔존은 0이어야 한다.
	// (수정 후: Signup 이 토큰 발급 성공 뒤에만 store 에 커밋 → 좀비 0)
	if orgsAfter != orgsBefore {
		t.Errorf("[T5 결함] 토큰 발급 실패 후 org 가 %d→%d 로 좀비 잔존 → 보상/롤백 필요",
			orgsBefore, orgsAfter)
	}
	if memAfter != memBefore {
		t.Errorf("[T5 결함] 토큰 발급 실패 후 membership 이 %d→%d 로 좀비 잔존 → 보상/롤백 필요",
			memBefore, memAfter)
	}
}
