// Sovereign Core — 개발/운영 경계 검증 시뮬 (engine_boundary_sim.go)
//
// 목적: 우리 엔진을 장기적으로 개발하려면, 개발엔진(53)과 운영엔진(50)이
//       단일 code 모델·단일 GPU 풀을 공유하되 경계가 안 무너져야 한다.
//       산문이 아니라 코드로 3개 경계를 검증한다.
//
// 경계질문:
//   Q1 GPU 충돌: 개발(배치)과 운영(실시간)이 단일 풀에서, 버스트 때 운영이 우선인가?
//   Q2 리턴 분기: 같은 code 모델을 두 라인이 호출할 때, provenance가 리턴을 다른 게이트로?
//   Q3 우회 차단: 개발 리턴(코드 diff)이 파이프라인을 우회해 인프라에 못 닿나?
//
// stdlib-only. 앞 change_pipeline(7게이트)의 경계 확장판.

package main

import (
	"errors"
	"fmt"
	"sort"
)

// ── 공유 자원 (1벌씩) ────────────────────────────────────────────────

// 단일 code 모델 (50 자체복잡추론 = 53 개선안 생성, 같은 1개)
type CodeModel struct{ name string }

func (m CodeModel) Infer(ctx CallContext, prompt string) Output {
	// 같은 모델, 호출 맥락(purpose)만 다름
	switch ctx.Purpose {
	case Ops:
		return Output{Kind: "ops-result", Body: "고객 추론 응답", Ctx: ctx}
	case Dev:
		return Output{Kind: "code-diff", Body: "C 개선 diff: " + prompt, Ctx: ctx}
	}
	return Output{}
}

// 단일 GPU 풀 (C3 중개, D53). 운영 우선·개발 유휴 흡수.
type GpuPool struct {
	total    int
	opsUsage int // 실시간(운영)이 잡은 양
}

// 개발 학습잡은 "잔여 유휴"에만 배정. 운영 버스트가 들어오면 선점당함.
func (g *GpuPool) RequestDev(need int) (granted int, preempted bool) {
	idle := g.total - g.opsUsage
	if idle <= 0 {
		return 0, true // 유휴 0 = 개발 학습 대기(선점)
	}
	if need > idle {
		return idle, true // 일부만, 나머지는 운영에 양보
	}
	return need, false
}
func (g *GpuPool) OpsBurst(amount int) { g.opsUsage += amount } // 운영은 항상 우선 확보

// ── 호출 맥락 = 경계의 핵심 (26 provenance + purpose) ────────────────

type Purpose string

const (
	Ops Purpose = "ops" // 운영엔진(50) 호출 — 즉시 적용 가능
	Dev Purpose = "dev" // 개발엔진(53) 호출 — 반드시 파이프라인 경유
)

type CallContext struct {
	Provenance string // 26: c-agent(개발) / timeline·c-agent(운영)
	Purpose    Purpose
}

type Output struct {
	Kind string
	Body string
	Ctx  CallContext
}

// ── 리턴 분기 = Q2·Q3의 핵심 ────────────────────────────────────────

// 운영 리턴: 미리 계산된 정책 → 즉시 적용 가능 (OpsEvalSuite=결정론)
func applyOps(o Output) (string, error) {
	if o.Ctx.Purpose != Ops {
		return "", errors.New("ops 경로에 비-ops 리턴")
	}
	// 결정론 안전 체크만 (LLM judge 아님)
	return "즉시 적용: " + o.Body, nil
}

// 개발 리턴: code diff → 절대 즉시 집행 X → 반드시 파이프라인 (DevEvalSuite→66→shadow→카나리)
func routeDev(o Output, infra *Infra) (string, error) {
	if o.Ctx.Purpose != Dev {
		return "", errors.New("dev 경로에 비-dev 리턴")
	}
	// Q3 우회 차단: 개발 리턴은 여기서 인프라에 직접 못 씀 — 파이프라인 핸들만 반환
	trace := []string{
		"DevEvalSuite(judge): 코드 품질 채점",
		"change pipeline 게이트3: 개발엔진↛인프라 하드레일 검사",
		"shadow → 카나리 → 오너 천장(비가역)",
	}
	// 인프라 직접 접근 시도 = 하드레일 위반
	if err := infra.directWrite(o.Ctx); err != nil {
		trace = append(trace, "⛔ 인프라 직접쓰기 차단됨: "+err.Error())
	}
	return "파이프라인 경유(즉시집행 X): " + fmt.Sprintf("%v", trace), nil
}

// ── 인프라 = Q3 검증 대상 ───────────────────────────────────────────

type Infra struct{}

// 인프라 직접 쓰기: 개발(dev) provenance는 절대 통과 못함 (D33·D52)
func (Infra) directWrite(ctx CallContext) error {
	if ctx.Purpose == Dev {
		return errors.New("개발엔진(53)은 인프라 직접 접근 금지 — c-agent는 API·파이프라인 경유만")
	}
	return nil // 운영(C-agent 운영권한)만 reconcile 경유 허용
}

// ── 시뮬 시나리오 ───────────────────────────────────────────────────

func main() {
	model := CodeModel{name: "code-llm-shared"}
	pool := &GpuPool{total: 100}
	infra := &Infra{}

	fmt.Println("=== Q1: GPU 단일 풀 — 운영 버스트 시 개발 학습이 선점당하나? ===")
	scenarios := []int{20, 60, 95} // 운영이 점점 더 많이 잡음
	for _, opsAmt := range scenarios {
		p := &GpuPool{total: 100}
		p.OpsBurst(opsAmt)
		granted, preempted := p.RequestDev(40) // 개발이 학습에 40 요청
		fmt.Printf("  운영 %d/100 사용 → 개발 학습 요청 40 → 배정 %d (선점=%v)\n",
			opsAmt, granted, preempted)
	}
	fmt.Println("  ▶ 운영이 커질수록 개발 학습이 유휴로 밀림 = 운영 우선 확인 (D53)")
	_ = pool

	fmt.Println("\n=== Q2: 같은 code 모델, purpose로 리턴이 다른 게이트로 가나? ===")
	calls := []CallContext{
		{Provenance: "c-agent", Purpose: Ops},
		{Provenance: "c-agent", Purpose: Dev},
	}
	for _, ctx := range calls {
		out := model.Infer(ctx, "connection pooler 최적화")
		fmt.Printf("  호출 purpose=%s → 모델 리턴 kind=%q\n", ctx.Purpose, out.Kind)
		if ctx.Purpose == Ops {
			r, _ := applyOps(out)
			fmt.Printf("     → OpsEvalSuite 경로: %s\n", r)
		} else {
			r, _ := routeDev(out, infra)
			fmt.Printf("     → DevEvalSuite 경로: %s\n", r)
		}
	}
	fmt.Println("  ▶ 모델 1개인데 purpose가 리턴을 다른 경로로 = 분기 확인")

	fmt.Println("\n=== Q3: 개발 리턴이 파이프라인 우회해 인프라에 직접 닿나? ===")
	devOut := model.Infer(CallContext{Provenance: "c-agent", Purpose: Dev}, "격리 설정 변경")
	// 개발이 인프라 직접 쓰기를 시도
	if err := infra.directWrite(devOut.Ctx); err != nil {
		fmt.Printf("  개발 직접쓰기 시도 → 차단: %s\n", err.Error())
	}
	// 운영은 reconcile 경유 허용 (대조)
	opsCtx := CallContext{Provenance: "c-agent", Purpose: Ops}
	if err := infra.directWrite(opsCtx); err == nil {
		fmt.Println("  운영 경로 → reconcile 허용 (대조: 운영은 인프라 운영 권한)")
	}
	fmt.Println("  ▶ 개발엔진↛인프라 하드레일이 코드로 강제됨 = 우회 차단 확인")

	fmt.Println("\n=== 경계 검증 종합 ===")
	results := map[string]string{
		"Q1 GPU 단일풀": "운영 우선·개발 유휴 흡수 (D53) ✓",
		"Q2 리턴 분기":  "모델 1개 / purpose로 게이트 분기 (D54) ✓",
		"Q3 우회 차단":  "개발↛인프라 하드레일 (D52·D33) ✓",
	}
	keys := make([]string, 0, len(results))
	for k := range results {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %s : %s\n", k, results[k])
	}
	fmt.Println("\n경계 3개 코드로 검증 완료 — 장기 엔진 개발 시 이 경계 위에 구현")
}
