// Sovereign Core — C 제어흐름 엔진 스켈레톤 (change_pipeline.go)
//
// 목적: 사용자/LLM의 변경 요청이 레이어를 넘나들며 안전하게 집행되는 경로를
//       "문서"가 아니라 실제 로직으로 고정한다.
//
// 핵심 안전 원칙 (파이프라인이 단순하면 대형사고 → 게이트로 방지):
//   · 입력은 단일 봉투(ChangeRequest)로 정규화. 다른 입구 없음.
//   · LLM은 제안(Diff)만 만든다. 판정도 집행도 안 한다. (Proposer)
//   · 판정 = 결정론(PolicyEngine=Rego 자리). 하드레일 위반은 승인 화면에도 안 감.
//   · 집행 = 상태기계 + shadow→카나리→롤아웃. 실패 시 자동 롤백.
//   · 전 과정 append-only 감사. (개선/오너/침해 사후 소급 근거)
//
// stdlib-only. 포트(interface) 뒤에 실제 구현(OPA/Vault/Executor)이 꽂힌다.
// v0.1 = 흐름 골격 + mock 어댑터. solver·집행 hot path는 자리만.
// v0.2 = 게이트3-0 구조 하드레일 추가: 개발엔진↛인프라 (D52 확정, 66 v0.3 소급 C).
// v0.3 = 게이트4 Planner mock → Solver 격상 (목적함수=인프라효율/SLA·주권·격리 하드제약, 50 §1·52).

package main

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ── 입력 타입 (26/21과 정합) ─────────────────────────────────────────

type Provenance string // 26 principal 5종

const (
	Owner       Provenance = "owner"        // 사람 오너 (Sandwich 천장)
	OwnerClient Provenance = "owner-client" // 오너 위임 클라이언트
	CAgent      Provenance = "c-agent"      // 내부 자동개선 (유일한 "개선" 출처, D33)
	Timeline    Provenance = "timeline"     // 테넌트 접속자
	AutoAgent   Provenance = "auto-agent"   // BYOC 아웃바운드
)

type ChangeKind string

const (
	CodeChange    ChangeKind = "code_change"    // 코드 수정
	FeatureToggle ChangeKind = "feature_toggle" // 기능 on/off
	OptionSet     ChangeKind = "option_set"     // 제품 옵션 정리
	ResourceOp    ChangeKind = "resource_op"    // 리소스 생성/재바인딩 (61)
	SelfImprove   ChangeKind = "self_improve"   // C 자동개선
)

type Risk int

const (
	RiskLow Risk = iota
	RiskMedium
	RiskHigh
	RiskIrreversible // 비가역 = 항상 오너 (66 천장)
)

func (r Risk) String() string {
	return [...]string{"low", "medium", "high", "irreversible"}[r]
}

// Diff = "무엇을 바꾸려는가". LLM이든 사람이든 결과물은 이 형태로 수렴.
type Diff struct {
	Summary    string
	Target     string // 대상 리소스/코드 경로
	Reversible bool
	Body       any // 실제 diff 페이로드(불투명)
}

// Purpose = 호출 목적 축 (D54: 모델 1개 공유, purpose로 경로 분기 / 26 provenance와 짝)
type Purpose string

const (
	PurposeOps Purpose = "ops" // 운영엔진(50) — reconcile 영역, 인프라 운영 권한
	PurposeDev Purpose = "dev" // 개발엔진(53) — 산출=제안(diff)만, 인프라 직접 접근 금지
)

// ChangeRequest = 단일 봉투. 모든 변경은 여기로만 들어온다.
type ChangeRequest struct {
	ID          string
	Origin      Provenance // 다른 건 몰라도 이 하나가 승인 강도를 정한다
	Purpose     Purpose    // ops/dev — 엔진 3분할 경계 축 (D52)
	Kind        ChangeKind
	Proposal    Diff
	TenantScope []string
	CreatedAt   time.Time
}

// ── 포트 (경계 추상화 — 실제 구현이 뒤에 꽂힘) ──────────────────────

// Proposer = LLM 자리. 의도→Diff 생성만. 절대 집행 인터페이스 없음.
type Proposer interface {
	Propose(intent string) (Diff, error)
}

// Authenticator = 26. Origin 검증·확정 (출처 위조 차단).
type Authenticator interface {
	Attest(req ChangeRequest) (Provenance, error)
}

// PolicyEngine = Rego 자리(결정론). SLA·정책·격리·주권 제약 질의.
type PolicyEngine interface {
	Evaluate(req ChangeRequest) PolicyDecision
}

type PolicyDecision struct {
	Allowed          bool
	HardRailViolated bool // 데이터·주권·규제·최소격리 = 즉사 (66·P8)
	Reasons          []string
}

// Planner = solver 자리. 통과 diff의 집행계획 + 영향·위험 산출.
type Planner interface {
	Plan(req ChangeRequest, dec PolicyDecision) (Plan, error)
}

type Plan struct {
	Steps           []string
	EstCostDelta    float64
	SLAImpact       string
	IsolationImpact string
	Risk            Risk
}

// Executor = 상태기계. shadow→카나리→롤아웃, 실패 시 롤백.
type Executor interface {
	Execute(ctx context.Context, plan Plan, audit *AuditLog) error
}

// AuditLog = append-only (61 D46). 소급 추적의 근거.
type AuditLog struct {
	entries []string
}

func (a *AuditLog) Append(gate, msg string) {
	a.entries = append(a.entries, fmt.Sprintf("  [%s] %s", gate, msg))
}
func (a *AuditLog) Dump() {
	for _, e := range a.entries {
		fmt.Println(e)
	}
}

// ── 파이프라인 = 7단 게이트 ─────────────────────────────────────────

type Pipeline struct {
	Auth   Authenticator
	Policy PolicyEngine
	Plan   Planner
	Exec   Executor
}

type Outcome string

const (
	Executed        Outcome = "EXECUTED"
	RejectedPolicy  Outcome = "REJECTED_HARDRAIL" // 게이트3 사망
	AwaitingOwner   Outcome = "AWAITING_OWNER"    // 게이트5 천장
	Failed          Outcome = "FAILED_ROLLED_BACK"
	RejectedAuthErr Outcome = "REJECTED_AUTH"
)

type Result struct {
	Outcome Outcome
	Audit   *AuditLog
}

func (p *Pipeline) Handle(ctx context.Context, req ChangeRequest) Result {
	audit := &AuditLog{}

	// 게이트1: 정규화 — 봉투가 아니면 입장 불가
	if req.ID == "" || req.Kind == "" {
		audit.Append("1-normalize", "malformed request rejected")
		return Result{RejectedAuthErr, audit}
	}
	audit.Append("1-normalize", fmt.Sprintf("kind=%s scope=%v", req.Kind, req.TenantScope))

	// 게이트2: 인증 — Origin 확정 (26). 개선/오너/침해 1차 판정.
	origin, err := p.Auth.Attest(req)
	if err != nil {
		audit.Append("2-authn", "attest failed → treated as intrusion")
		return Result{RejectedAuthErr, audit}
	}
	audit.Append("2-authn", fmt.Sprintf("origin=%s (%s)", origin, classify(origin)))

	// 게이트3-0: 구조 하드레일 (66 v0.3 소급 C, D52) — 어댑터(PolicyEngine) 이전에
	// 파이프라인 자체가 강제. mock/실구현 교체와 무관하게 절대 안 뚫림.
	//   개발엔진(53) 산출 = code diff 제안만. 인프라 직접 변경(ResourceOp,
	//   infra/ 경로 타깃)은 운영엔진(50) reconcile 영역 = dev purpose 즉사.
	if reason, violated := devInfraHardRail(req); violated {
		audit.Append("3-policy", "HARDRAIL(structural) 개발엔진↛인프라 (D52/66): "+reason)
		return Result{RejectedPolicy, audit}
	}

	// 게이트3: 제약검증 — 결정론(Rego). 하드레일 위반 = 즉사.
	dec := p.Policy.Evaluate(req)
	if dec.HardRailViolated {
		audit.Append("3-policy", "HARDRAIL violated → dies here, never reaches approval: "+join(dec.Reasons))
		return Result{RejectedPolicy, audit}
	}
	if !dec.Allowed {
		audit.Append("3-policy", "denied by policy: "+join(dec.Reasons))
		return Result{RejectedPolicy, audit}
	}
	audit.Append("3-policy", "constraints satisfied (SLA·isolation·sovereignty ok)")

	// 게이트4: 계획 — solver가 집행계획·영향·위험 산출.
	plan, err := p.Plan.Plan(req, dec)
	if err != nil {
		audit.Append("4-plan", "planning failed")
		return Result{Failed, audit}
	}
	audit.Append("4-plan", fmt.Sprintf("risk=%s costΔ=%.1f slaImpact=%s", plan.Risk, plan.EstCostDelta, plan.SLAImpact))

	// 게이트5: 승인 분기 — Origin×Kind×위험도. 여기 단순화하면 침해가 개선으로 위장 통과.
	if needsOwner(req, plan) {
		audit.Append("5-approve", "HIGH/irreversible → owner approval required (Sandwich ceiling, 66)")
		return Result{AwaitingOwner, audit}
	}
	audit.Append("5-approve", "auto-approved (low-risk, trusted origin)")

	// 게이트6: 집행 — shadow→카나리→롤아웃 (게이트7 롤백 내장)
	if err := p.Exec.Execute(ctx, plan, audit); err != nil {
		audit.Append("7-rollback", "failure detected → auto rolled back: "+err.Error())
		return Result{Failed, audit}
	}
	audit.Append("6-execute", "rolled out, error budget ok")
	return Result{Executed, audit}
}

// devInfraHardRail = 개발엔진↛인프라 (D52 확정, 66 v0.3 바닥, engine_boundary_sim Q3 검증분 이식).
// 결정론. 개발(purpose=dev) 요청이 인프라를 직접 대상으로 하면 사유와 true 반환.
func devInfraHardRail(req ChangeRequest) (string, bool) {
	if req.Purpose != PurposeDev {
		return "", false // 운영(ops)·미지정(레거시 봉투)은 이 레일 대상 아님
	}
	if req.Kind == ResourceOp {
		return "dev purpose가 ResourceOp 시도 — 인프라 변경은 운영엔진(50) reconcile 전용", true
	}
	if hasPrefix(req.Proposal.Target, "infra/") {
		return "dev purpose가 인프라 경로 타깃(" + req.Proposal.Target + ") — 제안(diff)만 허용", true
	}
	return "", false // dev의 code diff 제안(CodeChange·SelfImprove)은 정상 경로로 계속
}

func hasPrefix(s, p string) bool {
	return len(s) >= len(p) && s[:len(p)] == p
}

// 개선/오너요청/침해 = Origin 타입에서 1차 결정론 (26 발견의 구현)
func classify(o Provenance) string {
	switch o {
	case CAgent:
		return "IMPROVEMENT"
	case Owner, OwnerClient:
		return "OWNER-REQUEST"
	default:
		return "scoped"
	}
}

// 승인 게이트 로직 — 단순화가 사고인 지점을 명시적 규칙으로
func needsOwner(req ChangeRequest, plan Plan) bool {
	if plan.Risk == RiskIrreversible || plan.Risk == RiskHigh {
		return true // 비가역·고위험 = 무조건 천장
	}
	if req.Origin == AutoAgent && req.Kind == CodeChange {
		return true // BYOC 아웃바운드가 코드 바꾸려 하면 = 반드시 사람
	}
	return false // c-agent 저위험 자동개선 등만 자동 통과
}

// ── mock 어댑터 (실제 OPA/Executor가 대체) ─────────────────────────

type mockAuth struct{}

func (mockAuth) Attest(req ChangeRequest) (Provenance, error) {
	if req.Origin == "" {
		return "", errors.New("no provenance")
	}
	return req.Origin, nil
}

type mockPolicy struct{}

func (mockPolicy) Evaluate(req ChangeRequest) PolicyDecision {
	// Rego 자리: 여기선 규칙 몇 개를 결정론으로 시연
	// 하드레일: 데이터 리전 밖 이동·주권 위반 diff는 즉사
	if req.Kind == ResourceOp && contains(req.Proposal.Summary, "cross-region-key") {
		return PolicyDecision{false, true, []string{"key would leave region boundary (D24/25)"}}
	}
	if contains(req.Proposal.Summary, "disable-isolation") {
		return PolicyDecision{false, true, []string{"would breach minimum isolation tier (66)"}}
	}
	return PolicyDecision{Allowed: true}
}

// mockPlanner = v0.1 위험도 분류 mock. v0.2에서 Solver(solver.go)로 대체됨.
// 계보 참조용으로 남김 — 실제 wiring 은 NewSolver(mockCandidateGen{}).
type mockPlanner struct{}

func (mockPlanner) Plan(req ChangeRequest, _ PolicyDecision) (Plan, error) {
	r := RiskLow
	if req.Kind == CodeChange {
		r = RiskMedium
	}
	if !req.Proposal.Reversible {
		r = RiskIrreversible
	}
	if contains(req.Proposal.Summary, "schema-drop") {
		r = RiskHigh
	}
	return Plan{
		Steps:        []string{"shadow", "canary-5%", "rollout"},
		EstCostDelta: -12.5,
		SLAImpact:    "none",
		Risk:         r,
	}, nil
}

type mockExecutor struct{}

func (mockExecutor) Execute(ctx context.Context, plan Plan, audit *AuditLog) error {
	for _, s := range plan.Steps {
		audit.Append("6-execute", "step="+s+" ok")
		// 실제로는 각 단계 후 에러예산 검사 → 초과 시 return err (게이트7 트리거)
	}
	return nil
}

// ── 유틸 ────────────────────────────────────────────────────────────

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && indexOf(s, sub) >= 0
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
func join(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += "; "
		}
		out += s
	}
	return out
}

// ── 관통 시연 (도는지 검증) ─────────────────────────────────────────

func main() {
	p := &Pipeline{Auth: mockAuth{}, Policy: mockPolicy{}, Plan: NewSolver(mockCandidateGen{}), Exec: mockExecutor{}}
	ctx := context.Background()

	cases := []ChangeRequest{
		{ID: "r1", Origin: CAgent, Kind: SelfImprove,
			Proposal: Diff{Summary: "raise density on node-7 within tier", Reversible: true}},
		{ID: "r2", Origin: Owner, Kind: FeatureToggle,
			Proposal: Diff{Summary: "enable pgvector for tenant-A", Reversible: true}},
		{ID: "r3", Origin: AutoAgent, Kind: CodeChange,
			Proposal: Diff{Summary: "patch connection pooler", Reversible: true}},
		{ID: "r4", Origin: CAgent, Kind: ResourceOp,
			Proposal: Diff{Summary: "migrate cross-region-key for cost", Reversible: false}},
		{ID: "r5", Origin: OwnerClient, Kind: CodeChange,
			Proposal: Diff{Summary: "schema-drop legacy column", Reversible: false}},
		{ID: "r6", Origin: Timeline, Kind: FeatureToggle,
			Proposal: Diff{Summary: "disable-isolation for speed", Reversible: true}},
		// v0.2 하드레일 검증: 개발엔진↛인프라 (D52)
		{ID: "r7", Origin: CAgent, Purpose: PurposeDev, Kind: ResourceOp,
			Proposal: Diff{Summary: "dev engine tries to rebind node directly", Target: "infra/node-7", Reversible: true}},
		{ID: "r8", Origin: CAgent, Purpose: PurposeDev, Kind: CodeChange,
			Proposal: Diff{Summary: "propose pooler patch (diff only)", Target: "src/proxy/pooler.go", Reversible: true}},
	}

	for _, req := range cases {
		fmt.Printf("── %s  origin=%s kind=%s :: %q\n", req.ID, req.Origin, req.Kind, req.Proposal.Summary)
		res := p.Handle(ctx, req)
		res.Audit.Dump()
		fmt.Printf("  ▶ OUTCOME: %s\n\n", res.Outcome)
	}
	fmt.Println("관통 완료 — 8요청, 게이트별 분기 + 개발↛인프라 하드레일 검증")
}
