package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────
// 실행:
//   go run .              → P0 루프 walkthrough(웜풀 히트/미스 포함 관통 증명)
//   go run . -serve       → HTTP 서버(:8080)
// ─────────────────────────────────────────────────────────────────────────

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-serve" {
		serve()
		return
	}
	walkthrough()
}

func serve() {
	app := NewApp()
	srv := &Server{app: app}
	fmt.Println("Sovereign Core control-plane (mock) :8080")
	_ = http.ListenAndServe(":8080", srv.routes())
}

func walkthrough() {
	ctx := context.Background()
	app := NewApp()
	step := func(n int, msg string) { fmt.Printf("  [%d] %s\n", n, msg) }

	fmt.Println("── Sovereign Core · P0 루프 mock 관통 (웜풀 포함) ──")
	{
		ready, _, _, pol := app.warmpool.Stats()
		step(0, fmt.Sprintf("웜풀 정책=%s, 사전부팅 준비=%d개", pol, ready))
	}

	// 1. 가입(원자적)
	org, token, err := app.Signup(ctx, "acme-ai", "user_alice")
	must(err)
	step(1, fmt.Sprintf("signup → org=%s plan=%s kek=%s billing=%s", org.ID, org.Plan, org.KMSKeyID, org.BillingID))

	// 2. 토큰 검증 → org_id(RLS)
	claims, err := app.auth.VerifyToken(ctx, token)
	must(err)
	if claims.OrgID != org.ID {
		panic("RLS 경계 불일치")
	}
	step(2, fmt.Sprintf("verify token → org_id=%s role=%s (RLS 경계)", claims.OrgID, claims.Role))

	// 3. 프로젝트(async)
	op := app.CreateProject(ctx, org.ID, "prod")
	done, err := app.waitOp(org.ID, op.ID, time.Second)
	must(err)
	res := done.Result.(map[string]string)
	projID, rootBr := res["project_id"], res["root_branch_id"]
	step(3, fmt.Sprintf("create project(async) → project=%s root_branch=%s", projID, rootBr))

	// 4. 브랜치(CoW)
	op = app.CreateBranch(ctx, org.ID, projID, rootBr)
	done, err = app.waitOp(org.ID, op.ID, time.Second)
	must(err)
	brID := done.Result.(map[string]string)["branch_id"]
	step(4, fmt.Sprintf("create branch(CoW from %s) → branch=%s", rootBr, brID))

	// 5. 엔드포인트 기동 → 웜풀 히트 기대(boot 0)
	op = app.StartEndpoint(ctx, org.ID, brID, 0.25, 2)
	done, err = app.waitOp(org.ID, op.ID, time.Second)
	must(err)
	r5 := done.Result.(map[string]any)
	firstHit := r5["warm_hit"].(bool)
	firstBoot := r5["boot_ms"].(int64)
	epID := r5["endpoint_id"].(string)
	step(5, fmt.Sprintf("start endpoint → endpoint=%s warm_hit=%v boot=%dms", epID, firstHit, firstBoot))
	step(5, fmt.Sprintf("       connection_uri = %s", r5["connection_uri"]))

	// 6. SNI 라우팅 → 물리 백엔드
	sni := fmt.Sprintf("%s.%s.internal", epID, app.region)
	rid, addr, bootMS, err := app.ResolveConnection(ConnAttempt{SNI: sni})
	must(err)
	step(6, fmt.Sprintf("SNI 라우팅: %s → endpoint=%s backend=%s boot=%dms", sni, rid, addr, bootMS))

	// 6b. 식별 3중 폴백
	rid2, _, _, err := app.ResolveConnection(ConnAttempt{Options: "endpoint=" + epID})
	must(err)
	rid3, _, _, err := app.ResolveConnection(ConnAttempt{Password: "endpoint=" + epID + ";realpw"})
	must(err)
	if rid2 != epID || rid3 != epID {
		panic("폴백 체인 불일치")
	}
	fmt.Println("  [✓] endpoint 식별 3중 폴백: SNI=options=password 동일 도달")

	// 7. 사용량
	step(7, fmt.Sprintf("usage rollup = %v", app.Usage(org.ID)))

	// 8. suspend(STZ)
	must(app.SuspendEndpoint(ctx, org.ID, epID))
	ep, _ := app.store.getEndpoint(org.ID, epID)
	step(8, fmt.Sprintf("suspend endpoint → state=%s (scale-to-zero)", ep.State))

	// 8b. wake-on-connect → 웜풀에서 재확보
	_, wakeAddr, wakeMS, err := app.ResolveConnection(ConnAttempt{SNI: sni})
	must(err)
	step(8, fmt.Sprintf("재연결 → WakeHook backend=%s boot=%dms", wakeAddr, wakeMS))

	// 9. 웜풀 미스 강제: drain 후 신규 엔드포인트 → 콜드부팅 측정
	app.warmpool.Drain()
	op = app.StartEndpoint(ctx, org.ID, brID, 0.25, 2)
	done, err = app.waitOp(org.ID, op.ID, time.Second)
	must(err)
	r9 := done.Result.(map[string]any)
	coldHit := r9["warm_hit"].(bool)
	coldBoot := r9["boot_ms"].(int64)
	step(9, fmt.Sprintf("drain 후 기동 → warm_hit=%v boot=%dms (미스=콜드부팅)", coldHit, coldBoot))
	ready, hits, misses, pol := app.warmpool.Stats()
	step(9, fmt.Sprintf("웜풀 통계: 준비=%d 히트=%d 미스=%d 정책=%s", ready, hits, misses, pol))

	// ── 검증 ──
	if _, ok := app.store.getEndpoint("org_other", epID); ok {
		panic("RLS 누수!")
	}
	fmt.Println("  [✓] RLS 경계: 타 org 접근 차단 확인")

	app.store.idempStore("k1", "op_xyz")
	if id, ok := app.store.idempLookup("k1"); !ok || id != "op_xyz" {
		panic("멱등성 실패")
	}
	fmt.Println("  [✓] 멱등성: 동일 키 재요청 → 동일 op")

	if err := canTransition(endpointTransitions, EndpointDeleted, EndpointActive); err == nil {
		panic("금지 전이가 허용됨")
	}
	fmt.Println("  [✓] 상태머신: 금지 전이(deleted→active) 거부 확인")

	if !(firstHit && firstBoot == 0) {
		panic("웜풀 히트 기대 실패")
	}
	if !(!coldHit && coldBoot > 0) {
		panic("웜풀 미스 기대 실패")
	}
	fmt.Println("  [✓] 웜풀: 사전부팅 히트(boot=0) + drain 후 미스(boot>0) 확인")

	fmt.Println("── 관통 완료: signup→project→branch→endpoint→connection_uri→usage→suspend ──")
	fmt.Println("   (mock. 진짜 교체: mocks.go MockStorage.BootInstance/AttachBranch 만 엔진 호출로.)")
}

func must(err error) {
	if err != nil {
		fmt.Println("FAIL:", err)
		os.Exit(1)
	}
}
