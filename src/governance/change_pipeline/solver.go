// Sovereign Core — solver 스켈레톤 (solver.go)
//
// 목적: change_pipeline 게이트4의 Planner를 "위험도 분류 mock"에서
//       "목적함수 하 제약 최적화"로 격상한다 (90 클러스터2, 50 §1 소급 A).
//
// 우리 = 인프라 제작자. 그래서 목적함수가 앱 업체와 다르다:
//   최대화(objective):  인프라 효율 = 원가절감 + 밀도 + GPU활용 - 유휴
//   하드제약(constraint): SLA · 주권 · isolation_tier 격리 바닥 (넘을 수 없음, 66)
//   → 고객 SLA는 "목적"이 아니라 "제약". SLA를 목적함수에 넣으면 앱 업체로 샘.
//
// 이 파일 = solver "자리"의 진짜 골격. 실제 후보생성·비용모델은 H1 밀도 실측 후.
// stdlib-only. change_pipeline 과 같은 package main.

package main

import (
	"fmt"
	"math"
)

// ── 목적함수 = 인프라 효율 스코어 (50 §1 소급 A) ─────────────────────
// 높을수록 우리에게 좋음. 앱 업체면 여기 SLA만족이 들어가지만,
// 우리는 인프라 효율만. SLA·주권·격리는 아래 hardConstraints 로 분리.

type EfficiencyWeights struct {
	Cost      float64 // 원가절감 (costΔ 음수가 이득)
	Density   float64 // 밀도 상승 (노드당 타임라인↑)
	GpuUtil   float64 // GPU 활용률 상승
	IdlePenal float64 // 유휴 페널티 (유휴 낮을수록 좋음)
}

// 기본 가중치. 실제 계수는 H1 밀도 실측 후 재보정 (지금은 방향만 고정).
var defaultWeights = EfficiencyWeights{Cost: 1.0, Density: 0.6, GpuUtil: 0.4, IdlePenal: 0.5}

// 후보 계획 = 같은 요청을 집행하는 서로 다른 방법(노드 배치·밀도·GPU 배분).
// solver 는 제약을 통과하는 후보 중 목적함수 최대를 고른다.
type Candidate struct {
	Name          string
	Steps         []string
	CostDelta     float64 // 원가 변화 (음수 = 절감 = 이득)
	DensityDelta  float64 // 밀도 변화 (양수 = 이득)
	GpuUtilDelta  float64 // GPU 활용 변화
	IdleDelta     float64 // 유휴 변화 (양수 = 유휴 늘어남 = 손해)
	SLAHeadroom   float64 // SLA 여유 (음수 = SLA 위반 = 하드제약 위반)
	IsolationOK   bool    // isolation_tier 최소 바닥 충족?
	SovereigntyOK bool    // 주권(리전·키 경계) 충족?
	Reversible    bool
}

// score = 인프라 효율 목적함수. 제약(SLA/격리/주권)은 여기 안 들어감 — 게이트로 뺌.
func (w EfficiencyWeights) score(c Candidate) float64 {
	return w.Cost*(-c.CostDelta) + // 절감(음의 costΔ)이 +점수
		w.Density*c.DensityDelta +
		w.GpuUtil*c.GpuUtilDelta -
		w.IdlePenal*c.IdleDelta
}

// hardConstraints = 넘을 수 없는 바닥 (66). 하나라도 깨지면 후보 탈락.
// 목적함수와 분리 = 인프라 제작자 정체성의 코드적 표현.
func hardConstraints(c Candidate) (ok bool, reason string) {
	if c.SLAHeadroom < 0 {
		return false, "SLA 여유 음수 = SLA 하드제약 위반"
	}
	if !c.IsolationOK {
		return false, "isolation_tier 최소격리 바닥 미충족 (D50)"
	}
	if !c.SovereigntyOK {
		return false, "주권 경계(리전·키) 위반 (D24/25)"
	}
	return true, ""
}

// ── Solver = Planner 구현 (mockPlanner 대체) ────────────────────────
// 후보를 생성(CandidateGen) → 제약 통과분만 → 목적함수 최대 선택.

// CandidateGen = 요청을 집행하는 후보들을 생성. 진짜는 배치·밀도 탐색.
// 지금은 요청 성격에서 소수 후보를 결정론으로 뽑는 골격.
type CandidateGen interface {
	Generate(req ChangeRequest) []Candidate
}

type Solver struct {
	weights EfficiencyWeights
	gen     CandidateGen
}

func NewSolver(gen CandidateGen) *Solver {
	return &Solver{weights: defaultWeights, gen: gen}
}

// Plan = Planner 인터페이스 충족. 제약 통과 후보 중 목적함수 최대를 계획으로.
func (s *Solver) Plan(req ChangeRequest, dec PolicyDecision) (Plan, error) {
	cands := s.gen.Generate(req)
	if len(cands) == 0 {
		return Plan{}, fmt.Errorf("solver: 후보 0개 (집행 방법 없음)")
	}

	best := -1
	bestScore := math.Inf(-1)
	var rejected []string
	for i, c := range cands {
		ok, why := hardConstraints(c)
		if !ok {
			rejected = append(rejected, c.Name+": "+why)
			continue // 제약 위반 후보는 목적함수 평가조차 안 함
		}
		if sc := s.weights.score(c); sc > bestScore {
			bestScore, best = sc, i
		}
	}
	if best < 0 {
		return Plan{}, fmt.Errorf("solver: 모든 후보가 하드제약 탈락 [%v]", rejected)
	}

	c := cands[best]
	risk := riskOf(c)
	return Plan{
		Steps: append([]string{
			fmt.Sprintf("solver: %d후보 중 %q 선택 (효율점수=%.2f)", len(cands), c.Name, bestScore),
		}, c.Steps...),
		EstCostDelta:    c.CostDelta,
		SLAImpact:       fmt.Sprintf("headroom=%.2f", c.SLAHeadroom),
		IsolationImpact: boolStr(c.IsolationOK, "바닥충족", "위반"),
		Risk:            risk,
	}, nil
}

// 위험도 = 가역성·밀도변화폭에서 (mockPlanner 규칙 계승 + 후보 기반).
func riskOf(c Candidate) Risk {
	if !c.Reversible {
		return RiskIrreversible
	}
	if math.Abs(c.DensityDelta) > 0.5 {
		return RiskHigh // 큰 밀도 변화 = 고위험
	}
	return RiskLow
}

func boolStr(b bool, t, f string) string {
	if b {
		return t
	}
	return f
}

// ── mock 후보생성기 (실제 배치탐색이 대체) ──────────────────────────
// 요청 성격에서 2~3 후보를 뽑는다. 하드제약 검증 경로를 보이려 일부러
// "효율 높지만 제약 위반" 후보를 섞어, solver 가 그걸 탈락시키는지 검증.

type mockCandidateGen struct{}

func (mockCandidateGen) Generate(req ChangeRequest) []Candidate {
	rev := req.Proposal.Reversible
	switch req.Kind {
	case SelfImprove: // 밀도 최적화 성격
		return []Candidate{
			// A: 공격적 고밀도 — 효율 최고지만 격리 바닥 깨짐 → 탈락해야
			{Name: "aggressive-pack", Steps: []string{"repack node-7 to 200 timelines"},
				CostDelta: -30, DensityDelta: 0.9, GpuUtilDelta: 0.2, IdleDelta: -0.3,
				SLAHeadroom: 0.1, IsolationOK: false, SovereigntyOK: true, Reversible: rev},
			// B: 안전 고밀도 — 효율 양호 + 제약 통과 → 선택 기대
			{Name: "safe-pack", Steps: []string{"repack within isolation band", "shadow", "canary-5%", "rollout"},
				CostDelta: -18, DensityDelta: 0.4, GpuUtilDelta: 0.15, IdleDelta: -0.2,
				SLAHeadroom: 0.3, IsolationOK: true, SovereigntyOK: true, Reversible: rev},
			// C: 보수 — 효율 낮지만 안전. B보다 점수 낮아 안 뽑혀야
			{Name: "conservative", Steps: []string{"minor tune", "rollout"},
				CostDelta: -5, DensityDelta: 0.1, GpuUtilDelta: 0.05, IdleDelta: 0,
				SLAHeadroom: 0.5, IsolationOK: true, SovereigntyOK: true, Reversible: rev},
		}
	default: // 일반 변경 = 단일 후보
		return []Candidate{
			{Name: "direct", Steps: []string{"shadow", "canary-5%", "rollout"},
				CostDelta: -8, DensityDelta: 0.1, GpuUtilDelta: 0, IdleDelta: 0,
				SLAHeadroom: 0.4, IsolationOK: true, SovereigntyOK: true, Reversible: rev},
		}
	}
}
