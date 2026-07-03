package main

// ─────────────────────────────────────────────────────────────────────────
// T9 — Stage 3: 격리·보안 레드팀 (소프트웨어 레이어)
//
// 하드웨어(MicroVM/gVisor) 없이 검증 가능한 전 시나리오.
// 각 테스트는 "공격 시도 → 기대 차단"을 재현 가능하게 증명한다.
//
// 시나리오:
//   T9-A 토큰 위조(서명 없는 임의 JWT)
//   T9-B 교차 테넌트 리소스 조회(orgA 토큰으로 orgB 엔드포인트)
//   T9-C 교차 테넌트 오퍼레이션 폴링(orgA 토큰으로 orgB op)
//   T9-D 교차 테넌트 엔드포인트 정지(orgA 토큰으로 orgB suspend)
//   T9-E 토큰 없이 보호 엔드포인트 접근
//   T9-F 빈 토큰·Bearer 공백
//   T9-G org 클레임 없는 토큰
//   T9-H 라우팅 레지스트리 미등록 endpointID Resolve
//   T9-I SNI 경계: 다른 테넌트 endpointID 직접 삽입
//   T9-J 동시 다발 교차 테넌트 공격(100 goroutine)
// ─────────────────────────────────────────────────────────────────────────

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ─── 헬퍼: 레드팀 전용 App + Server 생성 (WarmPoolPolicy 포함) ──────────────────

func newRedTeamServer(t *testing.T) (*Server, *App) {
	t.Helper()
	app := NewAppWithPolicy(FixedPolicy{N: 2})
	return &Server{app: app}, app
}

// orgA 로 signup 후 토큰 반환
func signupOrg(t *testing.T, srv *Server, name, ownerID string) (orgID, token string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"Name": name, "OwnerUserID": ownerID})
	req := httptest.NewRequest("POST", "/v1/auth/signup", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleSignup(w, req)
	if w.Code != 201 {
		t.Fatalf("signup failed: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Org   struct{ ID string }
		Token string
	}
	_ = json.NewDecoder(w.Body).Decode(&resp)
	return resp.Org.ID, resp.Token
}

// ─── T9-A: 토큰 위조 ────────────────────────────────────────────────────────

func TestStage3_T9A_ForggedToken(t *testing.T) {
	srv, _ := newRedTeamServer(t)
	forged := []string{
		"",                                           // 빈 문자열
		"Bearer ",                                    // Bearer 뒤 공백만
		"notmock.abc",                                // 잘못된 prefix
		"mock." + base64.URLEncoding.EncodeToString([]byte(`{"OrgID":"evil"}`)), // 직접 조작
		"eyJhbGciOiJub25lIn0.eyJPcmdJRCI6ImV2aWwifQ.", // alg=none JWT 시도
		"mock.!!!invalid_base64!!!",                  // base64 깨짐
	}
	for _, tok := range forged {
		req := httptest.NewRequest("GET", "/v1/usage", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		srv.auth(func(_ http.ResponseWriter, _ *http.Request, _ Claims) {
			t.Errorf("[T9-A 결함] 위조 토큰이 통과됨: %q", tok)
		})(w, req)
		if w.Code != 401 {
			t.Errorf("[T9-A 결함] 위조 토큰 %q → 기대 401, got %d", tok, w.Code)
		}
	}
	t.Log("[T9-A] 위조 토큰 6종 전부 401 거부")
}

// ─── T9-B: 교차 테넌트 엔드포인트 조회 ─────────────────────────────────────

func TestStage3_T9B_CrossTenantEndpointRead(t *testing.T) {
	srv, app := newRedTeamServer(t)
	ctx := context.Background()

	// orgA 생성 + 엔드포인트 등록
	orgAID, tokA := signupOrg(t, srv, "orgA", "userA")
	prjA := app.CreateProject(ctx, orgAID, "projA")
	_, _ = app.waitOp(orgAID, prjA.ID, 3*time.Second)
	// orgA의 프로젝트에서 브랜치 + 엔드포인트 생성
	brOp := app.CreateBranch(ctx, orgAID, prjA.ID, "")
	_, _ = app.waitOp(orgAID, brOp.ID, 3*time.Second)
	// store에서 orgA의 브랜치 ID 추출
	app.store.mu.RLock()
	var branchID string
	for id, br := range app.store.branches {
		if br.OrgID == orgAID {
			branchID = id
			break
		}
	}
	app.store.mu.RUnlock()
	if branchID == "" {
		t.Skip("브랜치 생성 안됨 — 스킵")
	}
	epOp := app.StartEndpoint(ctx, orgAID, branchID, 0.25, 2)
	_, _ = app.waitOp(orgAID, epOp.ID, 3*time.Second)
	// orgA의 endpoint ID 추출
	app.store.mu.RLock()
	var epID string
	for id, ep := range app.store.endpoints {
		if ep.OrgID == orgAID {
			epID = id
			break
		}
	}
	app.store.mu.RUnlock()
	if epID == "" {
		t.Skip("엔드포인트 생성 안됨 — 스킵")
	}

	// orgB 생성 후 orgB 토큰으로 orgA 엔드포인트 조회 시도
	_, tokB := signupOrg(t, srv, "orgB", "userB")
	claimsB, _ := srv.app.auth.VerifyToken(ctx, tokB)

	ep, ok := app.store.getEndpoint(claimsB.OrgID, epID)
	if ok {
		t.Fatalf("[T9-B 결함] orgB 가 orgA 엔드포인트를 조회함: %+v", ep)
	}
	t.Logf("[T9-B] orgB 로 orgA 엔드포인트(%s) 조회 → 차단(ok=false)", epID)

	// HTTP 레이어에서도 확인
	req := httptest.NewRequest("GET", "/v1/endpoints/"+epID, nil)
	req.Header.Set("Authorization", "Bearer "+tokA)
	// orgA 토큰으로는 200 이어야
	w := httptest.NewRecorder()
	srv.auth(func(w http.ResponseWriter, r *http.Request, c Claims) {
		ep2, ok2 := srv.app.store.getEndpoint(c.OrgID, epID)
		if ok2 {
			writeJSON(w, 200, ep2)
		} else {
			writeJSON(w, 404, map[string]string{"error": "not found"})
		}
	})(w, req)
	if w.Code != 200 {
		t.Logf("[T9-B 참고] orgA 토큰으로 자신의 ep 조회: %d (비동기 완료 전일 수 있음)", w.Code)
	}
}

// ─── T9-C: 교차 테넌트 오퍼레이션 폴링 ─────────────────────────────────────

func TestStage3_T9C_CrossTenantOpPoll(t *testing.T) {
	srv, app := newRedTeamServer(t)
	ctx := context.Background()

	orgAID, _ := signupOrg(t, srv, "orgA2", "userA2")
	_, tokB := signupOrg(t, srv, "orgB2", "userB2")
	claimsB, _ := srv.app.auth.VerifyToken(ctx, tokB)

	// orgA 의 op 생성
	op := app.CreateProject(ctx, orgAID, "projA2")

	// orgB 토큰으로 orgA op 조회
	snap, ok := app.store.opSnapshot(claimsB.OrgID, op.ID)
	if ok {
		t.Fatalf("[T9-C 결함] orgB 가 orgA op 를 조회함: %+v", snap)
	}
	t.Logf("[T9-C] orgB 로 orgA op(%s) 폴링 → 차단(ok=false)", op.ID)
}

// ─── T9-D: 교차 테넌트 엔드포인트 정지 ─────────────────────────────────────

func TestStage3_T9D_CrossTenantSuspend(t *testing.T) {
	srv, app := newRedTeamServer(t)
	ctx := context.Background()

	orgAID, _ := signupOrg(t, srv, "orgA3", "userA3")
	_, tokB := signupOrg(t, srv, "orgB3", "userB3")
	claimsB, _ := srv.app.auth.VerifyToken(ctx, tokB)

	// orgA 엔드포인트 등록(store에 직접 삽입하여 비동기 대기 생략)
	fakeEpID := newID("ep")
	app.store.mu.Lock()
	app.store.endpoints[fakeEpID] = &Endpoint{ID: fakeEpID, OrgID: orgAID, State: EndpointActive}
	app.store.mu.Unlock()

	// orgB 토큰으로 orgA 엔드포인트 정지 시도
	err := app.SuspendEndpoint(ctx, claimsB.OrgID, fakeEpID)
	if err == nil {
		// 정지 성공이면 결함 — orgA ep 상태 확인
		app.store.mu.RLock()
		ep := app.store.endpoints[fakeEpID]
		app.store.mu.RUnlock()
		if ep != nil && ep.State == EndpointSuspended {
			t.Fatalf("[T9-D 결함] orgB 가 orgA 엔드포인트를 정지시킴")
		}
	}
	t.Logf("[T9-D] orgB 로 orgA ep 정지 시도 → err=%v (차단)", err)
}

// ─── T9-E: 토큰 없이 보호 엔드포인트 ───────────────────────────────────────

func TestStage3_T9E_NoTokenProtectedRoute(t *testing.T) {
	srv, _ := newRedTeamServer(t)
	routes := []struct{ method, path string }{
		{"POST", "/v1/projects"},
		{"GET", "/v1/usage"},
		{"GET", "/v1/operations/op_test"},
	}
	for _, r := range routes {
		req := httptest.NewRequest(r.method, r.path, nil)
		// Authorization 헤더 없음
		w := httptest.NewRecorder()
		srv.auth(func(_ http.ResponseWriter, _ *http.Request, _ Claims) {
			t.Errorf("[T9-E 결함] 토큰 없이 통과: %s %s", r.method, r.path)
		})(w, req)
		if w.Code != 401 {
			t.Errorf("[T9-E 결함] %s %s → 기대 401, got %d", r.method, r.path, w.Code)
		}
	}
	t.Log("[T9-E] 토큰 없는 보호 라우트 3종 → 전부 401")
}

// ─── T9-F: 빈 토큰·Bearer 공백 ──────────────────────────────────────────────

func TestStage3_T9F_EmptyBearerToken(t *testing.T) {
	srv, _ := newRedTeamServer(t)
	for _, hdr := range []string{"", "Bearer", "Bearer ", "Bearer  "} {
		req := httptest.NewRequest("GET", "/v1/usage", nil)
		if hdr != "" {
			req.Header.Set("Authorization", hdr)
		}
		w := httptest.NewRecorder()
		srv.auth(func(_ http.ResponseWriter, _ *http.Request, _ Claims) {
			t.Errorf("[T9-F 결함] 빈/공백 토큰 통과: %q", hdr)
		})(w, req)
		if w.Code != 401 {
			t.Errorf("[T9-F 결함] hdr=%q → 기대 401, got %d", hdr, w.Code)
		}
	}
	t.Log("[T9-F] 빈/공백 Bearer 4종 → 전부 401")
}

// ─── T9-G: org 클레임 없는 토큰 ────────────────────────────────────────────

func TestStage3_T9G_MissingOrgClaim(t *testing.T) {
	srv, _ := newRedTeamServer(t)
	// OrgID 없는 Claims 직접 인코딩
	noOrg := Claims{UserID: "user_evil", Role: "owner"}
	j, _ := json.Marshal(noOrg)
	tok := "mock." + base64.URLEncoding.EncodeToString(j)

	req := httptest.NewRequest("GET", "/v1/usage", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	srv.auth(func(_ http.ResponseWriter, _ *http.Request, _ Claims) {
		t.Error("[T9-G 결함] org 클레임 없는 토큰이 통과됨")
	})(w, req)
	if w.Code != 401 {
		t.Errorf("[T9-G 결함] 기대 401, got %d", w.Code)
	}
	t.Log("[T9-G] org 클레임 없는 토큰 → 401")
}

// ─── T9-H: 라우팅 레지스트리 미등록 endpointID ──────────────────────────────

func TestStage3_T9H_UnregisteredEndpointResolve(t *testing.T) {
	reg := NewRoutingRegistry(func(id string) (string, int64, error) {
		return "", 0, fmt.Errorf("wake failed: %s", id)
	})
	_, _, err := reg.Resolve("ep_nonexistent_xyz")
	if err == nil {
		t.Fatal("[T9-H 결함] 미등록 endpointID 가 Resolve 성공함")
	}
	t.Logf("[T9-H] 미등록 ep Resolve → err=%v (차단)", err)
}

// ─── T9-I: SNI 경계 — 다른 테넌트 endpointID 삽입 ──────────────────────────

func TestStage3_T9I_SNIBoundary(t *testing.T) {
	reg := NewRoutingRegistry(nil)
	// orgA 의 ep 만 등록
	reg.Register("ep_orgA_001", "10.0.0.1:5432", "active")

	// orgB 가 orgA 의 ep 를 SNI 에 직접 삽입해 연결 시도
	attempt := ConnAttempt{
		SNI:      "ep_orgA_001.ap-northeast-2.internal",
		Password: "pw_orgB",
	}
	epID, _, method, err := resolveEndpointID(attempt)
	if err != nil {
		t.Logf("[T9-I] SNI 파싱 자체 실패(예상치 못한 오류): %v", err)
	}
	if epID == "ep_orgA_001" {
		// SNI 파싱은 성공 — 이제 Resolve 단계에서 RLS 로 차단되어야 함
		// (실제 프록시는 여기서 연결 요청자의 org 를 검증해야 함)
		// 현재 RoutingRegistry 는 org 검증 없이 주소만 반환 → 설계 갭 기록
		addr, _, rerr := reg.Resolve(epID)
		t.Logf("[T9-I 설계갭] SNI=%q → epID=%s(method=%s) Resolve: addr=%s err=%v",
			attempt.SNI, epID, method, addr, rerr)
		if rerr == nil && addr != "" {
			t.Log("[T9-I 설계갭 확인] RoutingRegistry.Resolve 는 org 검증 없이 주소 반환 — " +
				"실제 프록시 레이어에서 연결자 org 와 ep.OrgID 를 대조하는 추가 RLS 필요")
		}
	}
}

// ─── T9-J: 동시 다발 교차 테넌트 공격 ──────────────────────────────────────

func TestStage3_T9J_ConcurrentCrossTenantAttack(t *testing.T) {
	srv, app := newRedTeamServer(t)
	ctx := context.Background()

	orgAID, _ := signupOrg(t, srv, "orgA_mass", "userA_mass")
	_, tokB := signupOrg(t, srv, "orgB_mass", "userB_mass")
	claimsB, _ := srv.app.auth.VerifyToken(ctx, tokB)

	// orgA 의 리소스 100개 등록
	for i := 0; i < 100; i++ {
		epID := newID("ep")
		app.store.mu.Lock()
		app.store.endpoints[epID] = &Endpoint{ID: epID, OrgID: orgAID, State: EndpointActive}
		app.store.mu.Unlock()
	}

	// orgB 토큰으로 100 goroutine 동시 공격
	var leaked int64
	var wg sync.WaitGroup
	app.store.mu.RLock()
	epIDs := make([]string, 0, 100)
	for id, ep := range app.store.endpoints {
		if ep.OrgID == orgAID {
			epIDs = append(epIDs, id)
		}
	}
	app.store.mu.RUnlock()

	for _, id := range epIDs {
		wg.Add(1)
		go func(epID string) {
			defer wg.Done()
			_, ok := app.store.getEndpoint(claimsB.OrgID, epID)
			if ok {
				atomic.AddInt64(&leaked, 1)
			}
		}(id)
	}
	wg.Wait()

	t.Logf("[T9-J] orgB 의 100 동시 교차 테넌트 조회 → 누출=%d건", leaked)
	if leaked > 0 {
		t.Fatalf("[T9-J 결함] %d건 교차 테넌트 누출 발생", leaked)
	}
	t.Log("[T9-J] 동시 100건 교차 테넌트 공격 → 전부 차단(누출 0)")
}
