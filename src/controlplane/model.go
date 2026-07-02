package main

import (
	"fmt"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────
// 도메인 모델 — DataModel v0.1 / Master v1.6 §1(나) 제품 기반 7개 중 골격부.
// 정체성 불변(D12): id·org_id·region·kms_key_id 는 생성 후 변경 불가.
// 구조 가변: plan·capacity·state 는 변동.
// ─────────────────────────────────────────────────────────────────────────

type Org struct {
	ID         string    `json:"id"`          // 🔒 불변
	Name       string    `json:"name"`        // 🔓
	Region     string    `json:"region"`      // 🔒 불변(데이터 레지던시)
	KMSKeyID   string    `json:"kms_key_id"`  // 🔒 불변(봉투암호화 루트)
	BillingID  string    `json:"billing_id"`  // platform 파이프라인
	Plan       string    `json:"plan"`        // 🔓 free|launch|scale
	CreatedAt  time.Time `json:"created_at"`
}

type Membership struct {
	OrgID  string `json:"org_id"`
	UserID string `json:"user_id"`
	Role   string `json:"role"` // owner|admin|member  (N:N: 한 유저가 여러 org)
}

type Project struct {
	ID        string    `json:"id"`        // 🔒
	OrgID     string    `json:"org_id"`    // 🔒 RLS 경계
	Name      string    `json:"name"`      // 🔓
	Region    string    `json:"region"`    // 🔒 (org.region 상속)
	RootBranch string   `json:"root_branch_id"`
	CreatedAt time.Time `json:"created_at"`
}

type Branch struct {
	ID       string    `json:"id"`        // 🔒
	OrgID    string    `json:"org_id"`    // 🔒 RLS
	ProjectID string   `json:"project_id"`
	ParentID string    `json:"parent_branch_id,omitempty"` // CoW 부모
	State    string    `json:"state"`     // 🔓 상태머신
	CreatedAt time.Time `json:"created_at"`
}

type Endpoint struct {
	ID            string    `json:"id"`        // 🔒  (SNI 라우팅 키)
	OrgID         string    `json:"org_id"`    // 🔒 RLS
	BranchID      string    `json:"branch_id"`
	State         string    `json:"state"`     // 🔓 상태머신
	AutoscaleMin  float64   `json:"autoscale_min_cu"`
	AutoscaleMax  float64   `json:"autoscale_max_cu"`
	SuspendAfterS int       `json:"suspend_timeout_s"`
	ConnectionURI string    `json:"connection_uri,omitempty"` // 핵심 산출물
	CreatedAt     time.Time `json:"created_at"`
}

// 비동기 작업 — ControlPlane API: POST는 operation_id 반환 → 폴링.
type Operation struct {
	ID       string    `json:"id"`
	OrgID    string    `json:"org_id"`
	Kind     string    `json:"kind"`   // create_project|create_branch|start_endpoint|...
	Status   string    `json:"status"` // running|succeeded|failed
	Result   any       `json:"result,omitempty"`
	Error    string    `json:"error,omitempty"`
	Created  time.Time `json:"created_at"`
}

// 사용량 미터링 이벤트(append-only).
type MeteringEvent struct {
	OrgID    string    `json:"org_id"`
	Kind     string    `json:"kind"` // cu_hours|gb_month|egress|branch_ops
	Value    float64   `json:"value"`
	At       time.Time `json:"at"`
}

// ─────────────────────────────────────────────────────────────────────────
// 노드 상태 머신 — 허용 전이만. (다음 산출물의 "상태 전이 규칙표"를 코드로)
// ─────────────────────────────────────────────────────────────────────────

const (
	BranchCreating = "creating"
	BranchReady    = "ready"
	BranchDeleted  = "deleted"

	EndpointCreating  = "creating"
	EndpointActive    = "active"
	EndpointSuspended = "suspended"
	EndpointDeleted   = "deleted"
)

var branchTransitions = map[string][]string{
	BranchCreating: {BranchReady, BranchDeleted},
	BranchReady:    {BranchDeleted},
	BranchDeleted:  {},
}

var endpointTransitions = map[string][]string{
	EndpointCreating:  {EndpointActive, EndpointDeleted},
	EndpointActive:    {EndpointSuspended, EndpointDeleted},
	EndpointSuspended: {EndpointActive, EndpointDeleted}, // 깨우기 = STZ
	EndpointDeleted:   {},
}

func canTransition(table map[string][]string, from, to string) error {
	for _, allowed := range table[from] {
		if allowed == to {
			return nil
		}
	}
	return fmt.Errorf("forbidden transition %s -> %s", from, to)
}
