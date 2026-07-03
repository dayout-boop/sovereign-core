package main

import (
	"context"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────
// infra_ports.go — 인프라 레이어 추가 포트 정의
//
// 설계 결정 (INFRA_DESIGN_DECISIONS.md 기반):
//   - 네트워크 타임아웃: 모든 외부 호출에 context.WithTimeout 강제 적용
//   - 비동기 이벤트 버스: 도메인 이벤트는 NATS JetStream 포트를 통해 발행
//   - 테넌트 프로비저닝: 논리적 격리(RLS + tenant_id) + Neon 브랜치 물리 격리
//   - GitOps: app-repo / infra-repo 분리 원칙 (코드 내 배포 직접 호출 금지)
// ─────────────────────────────────────────────────────────────────────────

// ─── 네트워크 회복성 설정 ──────────────────────────────────────────────────

// TimeoutConfig — 인프라 레이어별 타임아웃 설정.
// 모든 외부 호출은 이 설정을 기반으로 context.WithTimeout을 적용한다.
// 설계 원칙: 타임아웃 미설정 = 무한 대기 = 서킷 브레이커 미작동 = 연쇄 장애.
type TimeoutConfig struct {
	// PGCallTimeout — PG사 API 호출 타임아웃 (결제 승인, 환불 등).
	// 기준: Stripe 권장 최대 30초. 우리 기본값 10초.
	PGCallTimeout time.Duration

	// LLMCallTimeout — 외부 LLM API 호출 타임아웃.
	// 기준: OpenAI 스트리밍 최대 60초. 우리 기본값 30초.
	LLMCallTimeout time.Duration

	// StorageCallTimeout — 내부 스토리지 엔진(Neon pageserver) 호출 타임아웃.
	// 기준: 내부망 p99 < 100ms. 우리 기본값 5초 (여유 50배).
	StorageCallTimeout time.Duration

	// WebhookHandleTimeout — PG 웹훅 처리 타임아웃.
	// 기준: PG사는 보통 5초 내 응답 요구. 우리 기본값 4초.
	WebhookHandleTimeout time.Duration
}

// DefaultTimeoutConfig — 기본 타임아웃 설정.
// 프로덕션 배포 시 환경변수로 오버라이드 가능.
func DefaultTimeoutConfig() TimeoutConfig {
	return TimeoutConfig{
		PGCallTimeout:        10 * time.Second,
		LLMCallTimeout:       30 * time.Second,
		StorageCallTimeout:   5 * time.Second,
		WebhookHandleTimeout: 4 * time.Second,
	}
}

// ─── 비동기 이벤트 버스 포트 ──────────────────────────────────────────────

// DomainEvent — 도메인 이벤트 공통 구조체.
// 모든 핵심 도메인 이벤트는 이 구조를 통해 NATS JetStream으로 발행된다.
// 설계 원칙: 유실되면 안 되는 이벤트(결제 완료, 테넌트 프로비저닝 등)는
//   반드시 EventBusPort를 통해 발행하고, 단순 Go Channel을 사용하지 않는다.
type DomainEvent struct {
	// EventID — 멱등성 키. NATS JetStream의 Msg-Id 헤더로 전달.
	EventID string `json:"event_id"`
	// Subject — NATS 주제 (예: "sovereign.billing.payment.completed").
	Subject string `json:"subject"`
	// OrgID — 이벤트 발생 테넌트.
	OrgID string `json:"org_id"`
	// Payload — 이벤트 본문 (JSON 직렬화).
	Payload []byte `json:"payload"`
	// OccurredAt — 이벤트 발생 시각 (UTC).
	OccurredAt time.Time `json:"occurred_at"`
}

// EventBusPort — 분산 이벤트 버스 경계.
// 초기 구현: 인메모리(테스트용). 프로덕션: NATS JetStream 어댑터로 교체.
// 설계 원칙:
//   - Publish: 이벤트 발행. 멱등성 키(EventID)로 중복 발행 방지.
//   - Subscribe: 이벤트 구독. 핸들러가 오류 반환 시 JetStream이 재전달.
//   - 서킷 브레이커: NATS 연결 실패 시 인메모리 폴백으로 전환.
type EventBusPort interface {
	// Publish — 이벤트 발행. ctx 타임아웃 내 발행 실패 시 오류 반환.
	Publish(ctx context.Context, event DomainEvent) error
	// Subscribe — 주제 패턴 구독. 핸들러 반환 오류 시 재전달(at-least-once).
	Subscribe(ctx context.Context, subjectPattern string, handler func(DomainEvent) error) error
}

// ─── 핵심 도메인 이벤트 주제 상수 ────────────────────────────────────────

const (
	// 결제 이벤트
	EventPaymentCompleted  = "sovereign.billing.payment.completed"
	EventPaymentFailed     = "sovereign.billing.payment.failed"
	EventRefundIssued      = "sovereign.billing.refund.issued"
	EventSLACreditIssued   = "sovereign.billing.sla_credit.issued"

	// 테넌트 프로비저닝 이벤트
	EventTenantProvisioned = "sovereign.tenant.provisioned"
	EventTenantSuspended   = "sovereign.tenant.suspended"
	EventTenantDeleted     = "sovereign.tenant.deleted"

	// 컴퓨트 이벤트 (Scale-to-Zero)
	EventComputeWakeup     = "sovereign.compute.wakeup"
	EventComputeSuspended  = "sovereign.compute.suspended"
	EventComputeBootFailed = "sovereign.compute.boot.failed"

	// LLM 인가 이벤트
	EventLLMCallAuthorized = "sovereign.llm.call.authorized"
	EventLLMCallDenied     = "sovereign.llm.call.denied"
)

// ─── 테넌트 프로비저닝 포트 ───────────────────────────────────────────────

// TenantProvisionPort — 테넌트 인프라 프로비저닝 경계.
// 설계 원칙 (INFRA_DESIGN_DECISIONS.md §5):
//   - 기본 격리: Neon 브랜치 기반 논리적 DB 격리.
//   - 제어평면 메타데이터: tenant_id + RLS 논리 격리.
//   - 엔터프라이즈: 별도 Neon 프로젝트(물리 격리) 옵션.
type TenantProvisionPort interface {
	// ProvisionTenant — 신규 테넌트 인프라 프로비저닝.
	// 반환: 테넌트 전용 DB 연결 문자열, 오류.
	ProvisionTenant(ctx context.Context, orgID, planID string) (connStr string, err error)
	// DeprovisionTenant — 테넌트 인프라 해제 (데이터 보존 기간 30일 후 삭제).
	DeprovisionTenant(ctx context.Context, orgID string, immediate bool) error
	// GetTenantStatus — 테넌트 인프라 상태 조회.
	GetTenantStatus(ctx context.Context, orgID string) (status string, err error)
}

// ─── 서킷 브레이커 상태 ──────────────────────────────────────────────────

// CircuitState — 서킷 브레이커 상태.
// Closed(정상) → Open(차단) → HalfOpen(복구 시도) 순환.
type CircuitState int

const (
	CircuitClosed   CircuitState = iota // 정상 운영
	CircuitOpen                         // 장애 감지, 호출 차단
	CircuitHalfOpen                     // 복구 시도 중
)

// CircuitBreakerPort — 서킷 브레이커 상태 관리 경계.
// 외부 의존성(PG사, LLM API, NATS 등)별로 독립적인 서킷 브레이커를 관리한다.
type CircuitBreakerPort interface {
	// State — 현재 서킷 상태 조회.
	State(ctx context.Context, serviceName string) CircuitState
	// RecordSuccess — 성공 기록 (HalfOpen → Closed 전환 트리거).
	RecordSuccess(ctx context.Context, serviceName string)
	// RecordFailure — 실패 기록 (임계값 초과 시 Closed → Open 전환).
	RecordFailure(ctx context.Context, serviceName string, err error)
}
