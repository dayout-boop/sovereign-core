package main

import (
	"context"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────
// 포트 (D13 포트-어댑터) — 공급자 비종속 경계.
//
// 설계 원칙: 인터페이스 분리 원칙(ISP)
//   - 하나의 거대한 PaymentPort 대신, 기능별로 작은 포트로 분리한다.
//   - 각 어댑터는 자신이 지원하는 포트만 구현한다.
//   - 제품 1~4가 각각 다른 결제 흐름을 가질 때도 열린 구조를 유지한다.
//   - mock → 진짜(자체 엔진 / AWS KMS / Vault / Stripe / Toss)로 "국소 교체".
//
// 결제 포트 계층:
//   L0: CustomerPort      — 고객 등록 (모든 어댑터 구현)
//   L1: SubscriptionPort  — 구독 생성/변경 (구독 지원 어댑터만)
//   L2: InvoicePort       — 인보이스 생성/청구 (사용량 과금 어댑터만)
//   L3: RefundPort        — 환불/SLA 보상 (환불 지원 어댑터만)
//   L4: PaymentFailurePort— 결제 실패 유예 (전체 결제 어댑터)
//   L5: WebhookPort       — PG 웹훅 멱등성 (PG 연동 어댑터만)
//   L6: ExternalLLMAuthPort — L-5 외부 LLM 인가 (모든 LLM 호출 앞에 선 배치)
// ─────────────────────────────────────────────────────────────────────────

// ─── 인프라 포트 ──────────────────────────────────────────────────────────

// StoragePort — 자체호스팅 엔진 경계.
// 웜풀 도입으로 "컴퓨트 콜드부팅"(BootInstance)과 "브랜치 부착"(AttachBranch)을 분리.
type StoragePort interface {
	CreateBranch(ctx context.Context, orgID, projectID, parentBranchID string) (branchID string, err error)
	DeleteBranch(ctx context.Context, orgID, branchID string) error

	// BootInstance = 비어있는 컴퓨트 콜드부팅(비싼 작업). 진짜: PVM/Firecracker 부팅.
	BootInstance(ctx context.Context, region string) (instanceID, host string, bootMS int64, err error)
	// AttachBranch = 데워둔 컴퓨트에 브랜치 데이터 부착(싼 작업).
	AttachBranch(ctx context.Context, instanceID, branchID string) error

	SuspendEndpoint(ctx context.Context, orgID, endpointID string) error
	DeleteEndpoint(ctx context.Context, orgID, endpointID string) error
}

// AuthPort — 양방향 OIDC/SAML. SP(소비)+IdP(발급).
type AuthPort interface {
	IssueToken(ctx context.Context, orgID, userID, role string) (jwt string, err error)
	VerifyToken(ctx context.Context, jwt string) (claims Claims, err error)
	JWKS(ctx context.Context) (jwksJSON string, err error)
}

type Claims struct {
	OrgID  string // 안전 클레임에만(위변조 불가 경계)
	UserID string
	Role   string
}

// KmsPort — 봉투암호화. 평문 미보유. 테넌트별 KEK 프로비전.
type KmsPort interface {
	ProvisionTenantKEK(ctx context.Context, orgID string) (kmsKeyID string, err error)
}

// SecretPort — DB 자격증명 등.
type SecretPort interface {
	IssueDBCredential(ctx context.Context, orgID, endpointID string) (user, password string, err error)
}

// ─── 결제 공통 타입 ───────────────────────────────────────────────────────

// PGKind — 결제 게이트웨이 식별자. 다중 PG 어댑터 선택 키.
type PGKind string

const (
	PGStripe   PGKind = "stripe"   // 글로벌 카드 (Stripe)
	PGToss     PGKind = "toss"     // 국내 카드/계좌이체 (Toss Payments)
	PGKGInicis PGKind = "kginicis" // 국내 대형 가맹점 (KG이니시스)
	PGMock     PGKind = "mock"     // 테스트 전용
)

// SubscriptionPlan — 구독 플랜 정의.
// 설계 원칙: 연간 약정은 "대규모 선충전 + 보너스 크레딧" 방식으로 대체.
// 현금 환불 없음. 잔여 크레딧은 무기한 보관.
type SubscriptionPlan struct {
	PlanID       string  // "free" | "launch" | "scale" | "enterprise"
	PriceMonthly int64   // µc/월, 0=무료
	CommitMonths int     // 약정 개월 수 (0=월별)
	PenaltyRate  float64 // 위약금 비율 (0.0=없음, 0.5=잔여 약정의 50%)
}

// Invoice — 청구서 (사용량 기반 월말 정산).
type Invoice struct {
	ID          string           `json:"id"`
	OrgID       string           `json:"org_id"`
	PeriodStart time.Time        `json:"period_start"`
	PeriodEnd   time.Time        `json:"period_end"`
	LineItems   []InvoiceLineItem `json:"line_items"`
	TotalMicro  int64            `json:"total_micro"` // µc
	Status      string           `json:"status"`      // "draft"|"issued"|"paid"|"void"
	IssuedAt    time.Time        `json:"issued_at,omitempty"`
	PaidAt      time.Time        `json:"paid_at,omitempty"`
}

// InvoiceLineItem — 청구서 항목 (SKU별 사용량 × 단가).
type InvoiceLineItem struct {
	SKU         string  `json:"sku"`
	Quantity    float64 `json:"quantity"`
	UnitMicro   int64   `json:"unit_micro"`  // µc/단위
	TotalMicro  int64   `json:"total_micro"` // µc
	Description string  `json:"description"`
}

// RefundResult — 환불/보상 처리 결과.
type RefundResult struct {
	RefundID     string    `json:"refund_id"`
	OrgID        string    `json:"org_id"`
	AmountMicro  int64     `json:"amount_micro"`  // 실제 환불/보상된 µc
	PenaltyMicro int64     `json:"penalty_micro"` // 위약금 차감 µc (0=없음)
	Reason       string    `json:"reason"`
	ProcessedAt  time.Time `json:"processed_at"`
}

// ─── 결제 포트 L0: 고객 등록 ─────────────────────────────────────────────

// CustomerPort — 고객 등록 경계.
// 모든 결제 어댑터가 구현해야 하는 최소 포트.
// 멱등: 동일 orgID로 재호출 시 동일 billingID 반환.
type CustomerPort interface {
	CreateCustomer(ctx context.Context, orgID string) (billingID string, err error)
}

// ─── 결제 포트 L1: 구독 관리 ─────────────────────────────────────────────

// SubscriptionPort — 구독 생성/변경 경계.
// 구독 기능을 지원하는 어댑터만 구현한다.
// 플랜 변경 시 일할 계산(prorationMicro)을 반환한다.
type SubscriptionPort interface {
	CreateSubscription(ctx context.Context, orgID, billingID, planID string) (subscriptionID string, err error)
	ChangeSubscription(ctx context.Context, orgID, subscriptionID, newPlanID string) (prorationMicro int64, err error)
}

// ─── 결제 포트 L2: 인보이스/청구 ─────────────────────────────────────────

// InvoicePort — 사용량 기반 인보이스 생성 및 청구 경계.
// 사용량 과금(Pay-as-you-go)을 지원하는 어댑터만 구현한다.
type InvoicePort interface {
	CreateInvoice(ctx context.Context, orgID string, periodStart, periodEnd time.Time, items []InvoiceLineItem) (*Invoice, error)
	// ChargeInvoice: idempotencyKey로 중복 청구 방지.
	ChargeInvoice(ctx context.Context, invoiceID, idempotencyKey string) error
}

// ─── 결제 포트 L3: 환불/보상 ─────────────────────────────────────────────

// RefundPort — 환불 및 SLA 보상 크레딧 경계.
// 설계 원칙:
//   - 현금 환불은 EU 14일 철회권 예외 또는 SLA 위반 시에만 처리.
//   - SLA 위반 보상은 현금 환불이 아닌 크레딧 지급.
//   - penaltyRate: 약정 위약금 비율 (0.0~1.0).
type RefundPort interface {
	// Refund: 취소/환불 처리. cancelImmediate=true면 즉시 취소 + 일할 환불.
	Refund(ctx context.Context, orgID, subscriptionID string, cancelImmediate bool, penaltyRate float64, reason string) (*RefundResult, error)
	// IssueSLACredit: SLA 위반 자동 보상 크레딧 지급 (현금 환불 아님).
	IssueSLACredit(ctx context.Context, orgID string, compensationMicro int64, incidentID string) error
}

// ─── 결제 포트 L4: 결제 실패 유예 ───────────────────────────────────────

// PaymentFailurePort — 결제 실패 유예 기간 관리 경계.
// 결제 실패 시 즉시 서비스 중단이 아닌 유예 기간(기본 3일)을 부여한다.
type PaymentFailurePort interface {
	HandlePaymentFailure(ctx context.Context, orgID string, failedAt time.Time) (gracePeriodEnd time.Time, err error)
}

// ─── 결제 포트 L5: 웹훅 멱등성 ──────────────────────────────────────────

// WebhookPort — PG 웹훅 수신 멱등성 처리 경계.
//
// 설계 원칙:
//   - PG는 웹훅을 at-least-once로 전송한다. 중복 처리 = 이중 충전.
//   - MarkProcessed: 처음 처리 시 true 반환. 이미 처리된 이벤트면 false 반환.
//   - 구현체는 DB UNIQUE 제약 또는 Redis SETNX로 중복을 원천 차단한다.
type WebhookPort interface {
	// MarkProcessed: 웹훅 이벤트 ID를 처음 처리하면 true, 이미 처리됐으면 false.
	MarkProcessed(ctx context.Context, pgKind PGKind, eventID string) (isNew bool, err error)
	// GetProcessedResult: 이미 처리된 이벤트의 결과 조회 (멱등 재응답용).
	GetProcessedResult(ctx context.Context, pgKind PGKind, eventID string) (result string, found bool, err error)
	// SaveResult: 처리 결과 저장 (MarkProcessed 후 반드시 호출).
	SaveResult(ctx context.Context, pgKind PGKind, eventID, result string) error
}

// ─── 결제 포트 L6: 외부 LLM 인가 ────────────────────────────────────────

// ExternalLLMAuthPort — L-5 외부 LLM 호출 인가 스텁.
//
// 설계 원칙: 선 배치 강제
//   - 어느 제품에서든 외부 LLM 호출 전에 반드시 이 포트를 통과해야 한다.
//   - 실제 커널 구현 전에도 스텁(항상 허용)을 배치하여 인가 경로를 강제한다.
//   - 크레딧 선차감(Reserve) → 실제 호출 → 확정(Commit) 또는 반환(Release) 패턴.
type ExternalLLMAuthPort interface {
	// CheckAuth: 외부 LLM 호출 인가 확인 + 크레딧 선차감 예약.
	// 반환: 허용 여부, 예약된 크레딧(µc), 오류.
	CheckAuth(ctx context.Context, orgID, model string, estimatedTokens int64) (allowed bool, reservedMicro int64, err error)
	// CommitUsage: 실제 사용량 확정 (호출 완료 후 반드시 호출).
	CommitUsage(ctx context.Context, orgID, model string, actualTokens int64, reservedMicro int64) error
	// ReleaseReservation: 호출 실패 시 예약 크레딧 반환.
	ReleaseReservation(ctx context.Context, orgID string, reservedMicro int64) error
}

// ─── 결제 포트 L7: PG 라우터 ────────────────────────────────────────────

// PGRouterPort — 리전/테넌트 기준 PG 어댑터 동적 선택 경계.
// 설계 원칙:
//   - 비즈니스 로직은 PGRouterPort만 바라본다. 어떤 PG가 선택되는지 모른다.
//   - RegisterPG로 신규 PG를 등록하면 기존 코드 변경 없이 확장된다.
//   - 라우팅 기준: 테넌트 청구 국가 → KR=Toss, EU=StripeEU, 글로벌=StripeUS.
type PGRouterPort interface {
	// RouteByTenant — 테넌트 ID 기준 PG 어댑터 선택.
	RouteByTenant(ctx context.Context, orgID string) (PGKind, error)
	// RouteByRegion — 리전 코드 기준 PG 어댑터 선택.
	RouteByRegion(ctx context.Context, region string) (PGKind, error)
	// RegisterPG — 신규 리전-PG 매핑 등록 (런타임 확장).
	RegisterPG(region string, kind PGKind)
}

// ─── 운영자 포트 ──────────────────────────────────────────────────────────

// OperatorAction — 운영자 수기 처리 액션 유형.
type OperatorAction string

const (
	OpActionRefund          OperatorAction = "refund"           // 환불 승인
	OpActionCreditAdjust    OperatorAction = "credit_adjust"    // 크레딧 임의 조정
	OpActionReprocessDLQ    OperatorAction = "reprocess_dlq"    // DLQ 재처리
	OpActionSuspendTenant   OperatorAction = "suspend_tenant"   // 테넌트 강제 정지
	OpActionRestoreTenant   OperatorAction = "restore_tenant"   // 테넌트 복원
)

// AuditEvent — 운영자 수기 처리 감사 로그 항목.
type AuditEvent struct {
	EventID    string         `json:"event_id"`
	OperatorID string         `json:"operator_id"`
	Action     OperatorAction `json:"action"`
	TargetID   string         `json:"target_id"`  // orgID, refundID 등
	Reason     string         `json:"reason"`
	ApproverID string         `json:"approver_id,omitempty"` // 4-eyes 승인자
	OccurredAt time.Time      `json:"occurred_at"`
}

// OperatorPort — 운영자 수기 처리 경계.
// 설계 원칙:
//   - 모든 금액 변경 액션은 4-eyes(2인 승인) 필수.
//   - 모든 액션은 NATS audit 스트림에 불변 기록.
//   - 운영자는 외부 PG 대시보드에 직접 접근하지 않는다.
type OperatorPort interface {
	// RequestAction — 1차 운영자 처리 요청. 금액 변경 시 승인 대기 상태 반환.
	RequestAction(ctx context.Context, operatorID string, action OperatorAction, targetID, reason string, amountMicro int64) (requestID string, requiresApproval bool, err error)
	// ApproveAction — 2차 시니어 운영자 승인 (4-eyes). 승인 시 실제 처리 실행.
	ApproveAction(ctx context.Context, approverID, requestID string) error
	// RejectAction — 2차 승인자 거부.
	RejectAction(ctx context.Context, approverID, requestID, reason string) error
	// GetAuditLog — 감사 로그 조회.
	GetAuditLog(ctx context.Context, targetID string, from, to time.Time) ([]AuditEvent, error)
}

// ─── 알림 포트 ────────────────────────────────────────────────────────────

// NotificationChannel — 알림 발송 채널.
type NotificationChannel string

const (
	ChannelEmail  NotificationChannel = "email"
	ChannelPush   NotificationChannel = "push"
	ChannelInApp  NotificationChannel = "in_app"
)

// NotificationEvent — 알림 이벤트 유형.
type NotificationEvent string

const (
	NotifPaymentSucceeded    NotificationEvent = "payment.succeeded"
	NotifPaymentFailed       NotificationEvent = "payment.failed"
	NotifRefundCompleted     NotificationEvent = "refund.completed"
	NotifSubscriptionRenewing NotificationEvent = "subscription.renewing"
	NotifSubscriptionCancelled NotificationEvent = "subscription.cancelled"
	NotifTermsChanging       NotificationEvent = "terms.changing"
	NotifSLACompensated      NotificationEvent = "sla.compensated"
	NotifConsentRequired     NotificationEvent = "consent.required"
)

// NotificationRequest — 알림 발송 요청.
type NotificationRequest struct {
	OrgID    string              `json:"org_id"`
	Event    NotificationEvent   `json:"event"`
	Channels []NotificationChannel `json:"channels"`
	Payload  map[string]string   `json:"payload"`  // 템플릿 변수
	SendAt   time.Time           `json:"send_at"`  // 즉시=zero value
}

// NotificationPort — 이벤트 기반 알림 발송 경계.
// 설계 원칙:
//   - 고객의 가입 이메일 기준으로 NATS 이벤트 발생 시 자동 트리거.
//   - 채널 어댑터(Email/Push/InApp)는 교체 가능.
//   - ScheduleNotification으로 30일 전 약관 변경 고지 등 예약 발송 지원.
type NotificationPort interface {
	// SendTransactional — 즉시 발송 (결제 완료, 환불 완료 등).
	SendTransactional(ctx context.Context, req NotificationRequest) error
	// ScheduleNotification — 예약 발송 (갱신 7일 전, 약관 변경 30일 전 등).
	ScheduleNotification(ctx context.Context, req NotificationRequest) (scheduleID string, err error)
	// CancelScheduled — 예약 발송 취소.
	CancelScheduled(ctx context.Context, scheduleID string) error
	// GetDeliveryStatus — 발송 상태 조회.
	GetDeliveryStatus(ctx context.Context, notificationID string) (status string, err error)
}

// ─── 동의 수집 포트 ───────────────────────────────────────────────────────

// ConsentType — 동의 유형.
type ConsentType string

const (
	ConsentPriceIncrease  ConsentType = "price_increase"   // 가격 인상 (한국: 30일 전)
	ConsentTermsChange    ConsentType = "terms_change"      // 약관 변경 (EU: 명시적 재동의)
	ConsentDataPortability ConsentType = "data_portability" // 데이터 이전 (EU Data Act)
	ConsentAutoRenewal    ConsentType = "auto_renewal"      // 자동 갱신 동의
)

// ConsentStatus — 동의 상태.
type ConsentStatus string

const (
	ConsentPending  ConsentStatus = "pending"  // 동의 요청 발송됨, 응답 대기
	ConsentAccepted ConsentStatus = "accepted" // 동의 완료
	ConsentRejected ConsentStatus = "rejected" // 거부 (구독 취소 예정)
	ConsentExpired  ConsentStatus = "expired"  // 기한 초과 (자동 취소)
)

// ConsentRecord — 동의 수집 기록.
type ConsentRecord struct {
	ConsentID   string        `json:"consent_id"`
	OrgID       string        `json:"org_id"`
	Type        ConsentType   `json:"type"`
	Status      ConsentStatus `json:"status"`
	Deadline    time.Time     `json:"deadline"`
	RespondedAt time.Time     `json:"responded_at,omitempty"`
	CreatedAt   time.Time     `json:"created_at"`
}

// ConsentPort — 약관 변경 및 가격 인상 시 사전 동의 수집 경계.
// 설계 원칙:
//   - 한국: 가격 인상 30일 전 사전 동의 (전자상거래법 2025).
//   - EU: 약관 변경 시 명시적 재동의 (GDPR).
//   - 미동의 시 갱신 차단 → 구독 자동 취소.
type ConsentPort interface {
	// RequestConsent — 동의 요청 생성 및 알림 발송.
	RequestConsent(ctx context.Context, orgID string, consentType ConsentType, deadline time.Time) (consentID string, err error)
	// RecordConsent — 고객 동의/거부 응답 기록.
	RecordConsent(ctx context.Context, consentID string, accepted bool) error
	// GetConsentStatus — 특정 동의 요청 상태 조회.
	GetConsentStatus(ctx context.Context, consentID string) (*ConsentRecord, error)
	// ListPendingConsents — 특정 테넌트의 미완료 동의 목록 조회.
	ListPendingConsents(ctx context.Context, orgID string) ([]ConsentRecord, error)
}

// ─── 복합 포트 (편의용 조합) ──────────────────────────────────────────────

// FullPaymentPort — 전체 결제 기능을 지원하는 어댑터용 복합 포트.
// MultiPGPaymentAdapter가 이 인터페이스를 구현한다.
// 단순 어댑터(MarginBillingAdapter 등)는 이 인터페이스를 구현하지 않아도 된다.
type FullPaymentPort interface {
	CustomerPort
	SubscriptionPort
	InvoicePort
	RefundPort
	PaymentFailurePort
}
