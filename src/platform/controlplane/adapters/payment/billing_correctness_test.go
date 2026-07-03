package main

// ─────────────────────────────────────────────────────────────────────────
// Stage 2 — 정산 정합성 증명 (Money Correctness)
//
// 목적: 설계서의 "물리적 0건 / 0원 / 이중과금 0"이라는 서사(narrative)를
//       실제로 실행되는 반증 가능한 테스트(number)로 바꾼다.
//
// 원칙:
//   - 여기서의 "0"은 서사가 아니라 회계 무결성 요구사항이므로 진짜 0이어야 한다.
//   - 각 테스트는 하나의 명제만 죽인다.
//   - 기존 코드는 수정하지 않는다. 이 파일은 관측(observation) 도구다.
//
// 실행:
//   go test -race -run TestStage2 -v ./...
// ─────────────────────────────────────────────────────────────────────────

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ── 공통 헬퍼: 서버 + 온보딩된 org 1개 + 토큰 ──────────────────────────────

func newTestServer(t *testing.T) (*Server, *App) {
	t.Helper()
	app := NewApp()
	return &Server{app: app}, app
}

func onboard(t *testing.T, app *App, name string) (orgID, token string) {
	t.Helper()
	org, tok, err := app.Signup(context.Background(), name, "user_"+name)
	if err != nil {
		t.Fatalf("signup(%s) failed: %v", name, err)
	}
	return org.ID, tok
}

// HTTP 요청 헬퍼(멱등키 포함).
func doPost(srv *Server, token, path, idemKey, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, stringReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if idemKey != "" {
		r.Header.Set("Idempotency-Key", idemKey)
	}
	w := httptest.NewRecorder()
	srv.routes().ServeHTTP(w, r)
	return w
}

type sr struct {
	s string
	i int
}

func stringReader(s string) *sr { return &sr{s: s} }
func (r *sr) Read(p []byte) (int, error) {
	if r.i >= len(r.s) {
		return 0, fmt.Errorf("EOF")
	}
	n := copy(p, r.s[r.i:])
	r.i += n
	if r.i >= len(r.s) {
		return n, nil
	}
	return n, nil
}

// ─────────────────────────────────────────────────────────────────────────
// T1 — 멱등성 동시성: 같은 Idempotency-Key로 동시 N발 → operation 이 1개여야 한다.
//
// 근원: handlers.go startOp 은 idempLookup → make(op) → idempStore 가
//       3개의 분리된 store 호출이라 원자적이지 않다. 동시 요청 시 여러 op 가
//       생성될 수 있고, 이는 "1회 호출당 1회 과금" 계약을 깨는 이중과금이다.
// ─────────────────────────────────────────────────────────────────────────

func TestStage2_T1_IdempotencyConcurrency(t *testing.T) {
	srv, app := newTestServer(t)
	_, token := onboard(t, app, "t1")

	const N = 200
	const key = "same-key-abc"

	var wg sync.WaitGroup
	results := make([]string, N)
	codes := make([]int, N)

	start := make(chan struct{})
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start // 최대한 동시에 출발
			w := doPost(srv, token, "/v1/projects", key, `{"Name":"p"}`)
			codes[idx] = w.Code
			// operation_id 추출(단순 파싱)
			results[idx] = extractField(w.Body.String(), "operation_id")
		}(i)
	}
	close(start)
	wg.Wait()

	// 실제로 store 에 생성된 create_project operation 개수를 직접 센다(진실).
	app.store.mu.RLock()
	realOps := 0
	for _, op := range app.store.operations {
		if op.Kind == "create_project" {
			realOps++
		}
	}
	app.store.mu.RUnlock()

	// 응답으로 돌아간 서로 다른 operation_id 개수.
	distinct := map[string]bool{}
	for _, id := range results {
		if id != "" {
			distinct[id] = true
		}
	}

	t.Logf("[T1] 동시요청=%d, 응답 distinct operation_id=%d, store 실제 create_project op=%d",
		N, len(distinct), realOps)

	// 회계 무결성: 같은 멱등키 → 실제 생성 op 는 정확히 1개여야 한다.
	if realOps != 1 {
		t.Errorf("[T1 실패] 멱등키 1개인데 실제 operation 이 %d개 생성됨 → 이중과금 위험(중복 리소스/미터링)", realOps)
	}
	if len(distinct) != 1 {
		t.Errorf("[T1 실패] 클라이언트가 받은 operation_id 가 %d종류 → 멱등 계약 위반", len(distinct))
	}
}

// ─────────────────────────────────────────────────────────────────────────
// T2 — RLS 교차 테넌트 격리: org A 토큰으로 org B 자원 조회 시 반드시 404.
//     교차 조회가 뚫리면 "남의 자원에 과금" 또는 "정보 유출"이 된다.
// ─────────────────────────────────────────────────────────────────────────

func TestStage2_T2_CrossTenantIsolation(t *testing.T) {
	srv, app := newTestServer(t)
	_, tokenA := onboard(t, app, "orgA")
	orgB, tokenB := onboard(t, app, "orgB")

	// org B 가 프로젝트 생성.
	w := doPost(srv, tokenB, "/v1/projects", "", `{"Name":"b-proj"}`)
	if w.Code != 202 {
		t.Fatalf("[T2] orgB 프로젝트 생성 실패 code=%d", w.Code)
	}
	opID := extractField(w.Body.String(), "operation_id")

	// op 완료 대기.
	op, err := app.waitOp(orgB, opID, 2*time.Second)
	if err != nil || op.Status != "succeeded" {
		t.Fatalf("[T2] orgB op 미완료: %v status=%s", err, op.Status)
	}

	// org A 토큰으로 org B 의 operation 조회 시도 → 404 여야 함.
	rA := httptest.NewRequest("GET", "/v1/operations/"+opID, nil)
	rA.Header.Set("Authorization", "Bearer "+tokenA)
	wA := httptest.NewRecorder()
	srv.routes().ServeHTTP(wA, rA)

	t.Logf("[T2] orgB op를 orgA 토큰으로 조회 → code=%d (기대 404)", wA.Code)
	if wA.Code != 404 {
		t.Errorf("[T2 실패] 교차 테넌트 조회가 code=%d 로 허용됨 → RLS 경계 붕괴", wA.Code)
	}

	// 직접 store 스냅샷으로도 재확인(이중 검증).
	if _, ok := app.store.opSnapshot(orgB, opID); !ok {
		t.Errorf("[T2] orgB 소유 op 를 orgB 로도 못 읽음(설정 오류)")
	}
	if _, ok := app.store.opSnapshot("orgA-fake", opID); ok {
		t.Errorf("[T2 실패] 잘못된 orgID 로 op 조회가 성공함 → RLS 필터 미작동")
	}
	_ = orgB
}

// ─────────────────────────────────────────────────────────────────────────
// T3 — 미터링 원장 정합성: 서로 다른 org 가 동시에 다수 프로젝트를 만들 때
//     branch_ops 미터링 이벤트가 손실/중복 없이 정확히 org별로 집계되어야 한다.
//     (append-only 원장의 동시 기록 정확성 = 정산 근거의 신뢰성)
// ─────────────────────────────────────────────────────────────────────────

func TestStage2_T3_MeteringLedgerAccuracy(t *testing.T) {
	srv, app := newTestServer(t)

	const orgs = 10
	const perOrg = 20 // 각 org 가 만들 프로젝트 수(멱등키 각기 다름 → 전부 유효)

	type acct struct {
		id    string
		token string
	}
	accts := make([]acct, orgs)
	for i := 0; i < orgs; i++ {
		id, tok := onboard(t, app, fmt.Sprintf("t3org%d", i))
		accts[i] = acct{id, tok}
	}

	var wg sync.WaitGroup
	var accepted int64
	opIDs := make([][]string, orgs)
	for i := range opIDs {
		opIDs[i] = make([]string, perOrg)
	}

	for i := 0; i < orgs; i++ {
		for j := 0; j < perOrg; j++ {
			wg.Add(1)
			go func(oi, pj int) {
				defer wg.Done()
				key := fmt.Sprintf("t3-%d-%d", oi, pj) // 전부 고유 → 전부 유효 요청
				w := doPost(srv, accts[oi].token, "/v1/projects", key, `{"Name":"p"}`)
				if w.Code == 202 {
					atomic.AddInt64(&accepted, 1)
					opIDs[oi][pj] = extractField(w.Body.String(), "operation_id")
				}
			}(i, j)
		}
	}
	wg.Wait()

	// 모든 op 완료 대기.
	for i := 0; i < orgs; i++ {
		for j := 0; j < perOrg; j++ {
			if opIDs[i][j] == "" {
				continue
			}
			_, _ = app.waitOp(accts[i].id, opIDs[i][j], 3*time.Second)
		}
	}
	// 비동기 미터링 기록이 반영되도록 짧게 안정화.
	time.Sleep(50 * time.Millisecond)

	t.Logf("[T3] 총요청=%d, 202 accepted=%d", orgs*perOrg, accepted)

	// org별 미터링 집계 검증: 각 org 는 정확히 perOrg 개의 branch_ops 를 가져야 한다.
	totalLedger := 0.0
	for i := 0; i < orgs; i++ {
		u := app.Usage(accts[i].id)
		got := u["branch_ops"]
		totalLedger += got
		if int(got) != perOrg {
			t.Errorf("[T3 실패] org%d branch_ops 미터링=%v, 기대=%d (손실/중복)", i, got, perOrg)
		}
	}
	want := float64(orgs * perOrg)
	if totalLedger != want {
		t.Errorf("[T3 실패] 전체 미터링 원장 합=%v, 기대=%v → 원장 정합성 붕괴", totalLedger, want)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// T4 — 원자적 온보딩(장애주입): billing/kms 실패 시 org 가 부분 생성되면 안 된다.
//     (고아 org = 과금 불가능한 리소스 = 재정 누수. Signup 은 all-or-nothing 이어야)
//
// 실패 주입은 stock mock 이 못 하므로 테스트 로컬 mock 을 만들어 App 에 꽂는다.
// ─────────────────────────────────────────────────────────────────────────

type failingKms struct{}

func (failingKms) ProvisionTenantKEK(_ context.Context, _ string) (string, error) {
	return "", fmt.Errorf("kms down")
}

type failingPayment struct{}

func (failingPayment) CreateCustomer(_ context.Context, _ string) (string, error) {
	return "", fmt.Errorf("billing provider down")
}

func TestStage2_T4_AtomicOnboarding(t *testing.T) {
	// 케이스 A: KMS 실패 → org 미생성.
	{
		app := NewApp()
		app.kms = failingKms{}
		before := countOrgs(app)
		_, _, err := app.Signup(context.Background(), "kmsfail", "u1")
		after := countOrgs(app)
		t.Logf("[T4-KMS] err=%v, orgs before=%d after=%d", err, before, after)
		if err == nil {
			t.Errorf("[T4-KMS 실패] KMS 다운인데 Signup 이 성공 반환")
		}
		if after != before {
			t.Errorf("[T4-KMS 실패] KMS 실패에도 org 가 %d→%d 로 부분 생성됨(고아 리소스)", before, after)
		}
	}

	// 케이스 B: billing 실패 → org 가 store 에 남으면 안 된다(현재 구현 관측).
	{
		app := NewApp()
		app.payment = failingPayment{}
		before := countOrgs(app)
		_, _, err := app.Signup(context.Background(), "billfail", "u1")
		after := countOrgs(app)
		t.Logf("[T4-BILL] err=%v, orgs before=%d after=%d", err, before, after)
		if err == nil {
			t.Errorf("[T4-BILL 실패] billing 다운인데 Signup 이 성공 반환")
		}
		// 이 단언이 실패하면: billing 실패가 KEK 프로비전 '이후'라
		// org 객체가 이미 만들어졌지만 store.put 전에 반환되는지 등
		// 원자성 경계를 점검해야 한다는 신호다.
		if after != before {
			t.Errorf("[T4-BILL 관측] billing 실패 시 org 가 %d→%d 로 남음 → 보상 트랜잭션 필요 신호", before, after)
		}
	}
}

// ── 소도구 ────────────────────────────────────────────────────────────────

func countOrgs(app *App) int {
	app.store.mu.RLock()
	defer app.store.mu.RUnlock()
	return len(app.store.orgs)
}

// extractField — 의존 없는 초경량 JSON 값 추출("key":"value" 형태 전용).
func extractField(body, key string) string {
	needle := "\"" + key + "\":\""
	i := indexOf(body, needle)
	if i < 0 {
		return ""
	}
	i += len(needle)
	j := i
	for j < len(body) && body[j] != '"' {
		j++
	}
	return body[i:j]
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// 컴파일러가 net/http 를 쓰이는지 확인용(라우팅 상수 참조).
var _ = http.StatusUnauthorized
