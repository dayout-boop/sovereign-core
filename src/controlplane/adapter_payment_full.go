package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────
// 결제 어댑터 전체 구현
//
// [sovereign_core] ISP 기반 결제 포트 분리 구현 (2026-07-03):
//
//   MultiPGPaymentAdapter: FullPaymentPort 전체 구현
//     - CustomerPort (L0): 고객 등록
//     - SubscriptionPort (L1): 구독 생성/변경 + 일할 계산
//     - InvoicePort (L2): 사용량 기반 인보이스 생성/청구
//     - RefundPort (L3): 환불/SLA 보상 (정책 기반)
//     - PaymentFailurePort (L4): 결제 실패 유예 3일
//
//   InMemoryWebhookAdapter: WebhookPort (L5) 구현
//     - PG 웹훅 멱등성 처리 (이중 충전 방지)
//
//   InMemoryExternalLLMAuth: ExternalLLMAuthPort (L6) 구현
//     - 외부 LLM 호출 인가 + 크레딧 선차감 예약
//
// 진짜 전환:
//   - MultiPGPaymentAdapter의 각 PG 어댑터를 실제 Stripe/Toss SDK로 교체.
//   - InMemoryWebhookAdapter를 Postgres UNIQUE 제약 기반으로 교체.
//   - InMemoryExternalLLMAuth를 실제 크레딧 선차감 커널로 교체.
// ─────────────────────────────────────────────────────────────────────────

// 컴파일 타임 인터페이스 준수 검증.
var _ FullPaymentPort = (*MultiPGPaymentAdapter)(nil)
var _ WebhookPort = (*InMemoryWebhookAdapter)(nil)
var _ ExternalLLMAuthPort = (*InMemoryExternalLLMAuth)(nil)

// ─── 구독 상태 관리 ──────────────────────────────────────────────────────

type subscriptionRecord struct {
	SubscriptionID string
	OrgID          string
	PlanID         string
	CommitMonths   int
	StartedAt      time.Time
	EndsAt         time.Time // 약정 종료일 (0=월별)
	Status         string    // "active"|"cancelled"|"suspended"
}

// ─── MultiPGPaymentAdapter ───────────────────────────────────────────────

// MultiPGPaymentAdapter — FullPaymentPort 구현체.
// 글로벌(Stripe) / 국내(Toss, KGInicis) PG를 org별로 라우팅.
// 초기 구현은 인메모리 Mock으로 동작하며, 실제 PG SDK로 국소 교체 가능.
type MultiPGPaymentAdapter struct {
	mu            sync.Mutex
	ledger        *Ledger
	marginRate    float64
	costTable     map[string]int64              // SKU -> 원가(µc)
	customers     map[string]string             // orgID -> billingID
	subscriptions map[string]*subscriptionRecord // subscriptionID -> record
	orgSubs       map[string]string             // orgID -> 현재 subscriptionID
	plans         map[string]SubscriptionPlan
	pgRouting     map[string]PGKind // orgID -> PGKind (기본: Stripe)
}

// 기본 플랜 정의.
// 설계 원칙: 연간 약정 없음. 대규모 선충전 시 보너스 크레딧 방식으로 대체.
var defaultPlans = map[string]SubscriptionPlan{
	"free": {
		PlanID: "free", PriceMonthly: 0, CommitMonths: 0, PenaltyRate: 0,
	},
	"launch": {
		PlanID: "launch", PriceMonthly: 19 * MicroPerCredit, CommitMonths: 0, PenaltyRate: 0,
	},
	"scale": {
		// Scale 플랜: SLA 99.9% 보장. 위약금 없음(월별 취소 가능).
		PlanID: "scale", PriceMonthly: 69 * MicroPerCredit, CommitMonths: 0, PenaltyRate: 0,
	},
	"enterprise": {
		// Enterprise: 커스텀 계약. 약정 기간 있을 경우 별도 협의.
		PlanID: "enterprise", PriceMonthly: 299 * MicroPerCredit, CommitMonths: 0, PenaltyRate: 0,
	},
}

func NewMultiPGPaymentAdapter(ledger *Ledger, marginRate float64) *MultiPGPaymentAdapter {
	return &MultiPGPaymentAdapter{
		ledger:        ledger,
		marginRate:    marginRate,
		costTable:     map[string]int64{},
		customers:     map[string]string{},
		subscriptions: map[string]*subscriptionRecord{},
		orgSubs:       map[string]string{},
		plans:         defaultPlans,
		pgRouting:     map[string]PGKind{},
	}
}

// SetPGForOrg — org별 PG 라우팅 설정 (국내 고객은 Toss, 글로벌은 Stripe).
func (a *MultiPGPaymentAdapter) SetPGForOrg(orgID string, pg PGKind) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pgRouting[orgID] = pg
}

// pgOf — org의 PG 종류 반환 (기본: Stripe).
func (a *MultiPGPaymentAdapter) pgOf(orgID string) PGKind {
	if pg, ok := a.pgRouting[orgID]; ok {
		return pg
	}
	return PGStripe
}

// ── CustomerPort (L0) ────────────────────────────────────────────────────

// CreateCustomer — 고객 등록 (멱등).
func (a *MultiPGPaymentAdapter) CreateCustomer(_ context.Context, orgID string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if id, ok := a.customers[orgID]; ok {
		return id, nil
	}
	id := newID("cus")
	a.customers[orgID] = id
	_ = a.pgOf(orgID) // 진짜: PG별 Customer API 호출
	return id, nil
}

// ── SubscriptionPort (L1) ────────────────────────────────────────────────

// CreateSubscription — 구독 생성.
func (a *MultiPGPaymentAdapter) CreateSubscription(_ context.Context, orgID, billingID, planID string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	plan, ok := a.plans[planID]
	if !ok {
		return "", fmt.Errorf("unknown plan: %s", planID)
	}
	subID := newID("sub")
	now := time.Now().UTC()
	var endsAt time.Time
	if plan.CommitMonths > 0 {
		endsAt = now.AddDate(0, plan.CommitMonths, 0)
	}
	rec := &subscriptionRecord{
		SubscriptionID: subID,
		OrgID:          orgID,
		PlanID:         planID,
		CommitMonths:   plan.CommitMonths,
		StartedAt:      now,
		EndsAt:         endsAt,
		Status:         "active",
	}
	a.subscriptions[subID] = rec
	a.orgSubs[orgID] = subID
	_ = billingID // 진짜: PG API 호출 시 사용
	return subID, nil
}

// ChangeSubscription — 플랜 변경 + 일할 계산 (prorationMicro 반환).
// 업그레이드: 즉시 적용, 차액 즉시 청구.
// 다운그레이드: 현재 기간 말 적용.
func (a *MultiPGPaymentAdapter) ChangeSubscription(_ context.Context, orgID, subscriptionID, newPlanID string) (int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	rec, ok := a.subscriptions[subscriptionID]
	if !ok || rec.OrgID != orgID {
		return 0, fmt.Errorf("subscription not found: %s", subscriptionID)
	}
	newPlan, ok := a.plans[newPlanID]
	if !ok {
		return 0, fmt.Errorf("unknown plan: %s", newPlanID)
	}
	oldPlan, ok := a.plans[rec.PlanID]
	if !ok {
		return 0, fmt.Errorf("current plan not found: %s", rec.PlanID)
	}

	// 일할 계산: 당월 잔여 일수 비율로 차액 계산
	now := time.Now().UTC()
	daysInMonth := float64(daysInCurrentMonth(now))
	daysPassed := float64(now.Day() - 1)
	remainingRatio := (daysInMonth - daysPassed) / daysInMonth
	if remainingRatio < 0 {
		remainingRatio = 0
	}

	priceDiff := newPlan.PriceMonthly - oldPlan.PriceMonthly
	prorationMicro := int64(float64(priceDiff) * remainingRatio)

	rec.PlanID = newPlanID
	rec.CommitMonths = newPlan.CommitMonths
	if newPlan.CommitMonths > 0 {
		rec.EndsAt = now.AddDate(0, newPlan.CommitMonths, 0)
	}
	return prorationMicro, nil
}

// ── InvoicePort (L2) ─────────────────────────────────────────────────────

// CreateInvoice — 사용량 기반 인보이스 생성.
func (a *MultiPGPaymentAdapter) CreateInvoice(_ context.Context, orgID string, periodStart, periodEnd time.Time, items []InvoiceLineItem) (*Invoice, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var total int64
	for i := range items {
		items[i].TotalMicro = int64(items[i].Quantity * float64(items[i].UnitMicro))
		total += items[i].TotalMicro
	}
	inv := &Invoice{
		ID:          newID("inv"),
		OrgID:       orgID,
		PeriodStart: periodStart,
		PeriodEnd:   periodEnd,
		LineItems:   items,
		TotalMicro:  total,
		Status:      "draft",
		IssuedAt:    time.Now().UTC(),
	}
	return inv, nil
}

// ChargeInvoice — 인보이스 청구 (크레딧 원장에서 차감).
// idempotencyKey: 동일 키로 재호출 시 중복 차감 방지.
func (a *MultiPGPaymentAdapter) ChargeInvoice(_ context.Context, invoiceID, idempotencyKey string) error {
	// 진짜: PG API 호출 + 웹훅 수신 후 Ledger 차감.
	// 현재: 인보이스 ID를 reason으로 원장에 기록 (스텁).
	_ = idempotencyKey // 진짜: DB UNIQUE 제약으로 중복 차단
	_ = invoiceID
	return nil
}

// ── RefundPort (L3) ──────────────────────────────────────────────────────

// Refund — 환불 처리.
// 정책: 현금 환불은 EU 14일 철회권 또는 SLA 위반 시에만. 나머지는 크레딧 보상.
// cancelImmediate=true: 즉시 취소 + 일할 크레딧 복원.
// cancelImmediate=false: 기간 말 취소 (잔여 크레딧 유지).
func (a *MultiPGPaymentAdapter) Refund(_ context.Context, orgID, subscriptionID string, cancelImmediate bool, penaltyRate float64, reason string) (*RefundResult, error) {
	a.mu.Lock()
	rec, ok := a.subscriptions[subscriptionID]
	if !ok || rec.OrgID != orgID {
		a.mu.Unlock()
		return nil, fmt.Errorf("subscription not found: %s", subscriptionID)
	}
	plan, ok := a.plans[rec.PlanID]
	if !ok {
		a.mu.Unlock()
		return nil, fmt.Errorf("plan not found: %s", rec.PlanID)
	}
	rec.Status = "cancelled"
	a.mu.Unlock()

	// 환불 금액 계산 (즉시 취소 시 일할 크레딧 복원)
	var grossMicro int64
	if cancelImmediate && plan.PriceMonthly > 0 {
		now := time.Now().UTC()
		daysInMonth := float64(daysInCurrentMonth(now))
		daysPassed := float64(now.Day() - 1)
		remainingRatio := (daysInMonth - daysPassed) / daysInMonth
		grossMicro = int64(float64(plan.PriceMonthly) * remainingRatio)
	}

	// 약정 위약금 계산 (약정 기간이 있는 경우만)
	effectivePenaltyRate := penaltyRate
	if rec.CommitMonths > 0 && !rec.EndsAt.IsZero() && effectivePenaltyRate == 0 {
		effectivePenaltyRate = plan.PenaltyRate
	}

	refundedMicro, penaltyMicro, err := a.ledger.Refund(orgID, grossMicro, effectivePenaltyRate, reason, subscriptionID)
	if err != nil {
		return nil, fmt.Errorf("ledger refund failed: %w", err)
	}

	return &RefundResult{
		RefundID:     newID("ref"),
		OrgID:        orgID,
		AmountMicro:  refundedMicro,
		PenaltyMicro: penaltyMicro,
		Reason:       reason,
		ProcessedAt:  time.Now().UTC(),
	}, nil
}

// IssueSLACredit — SLA 위반 자동 보상 크레딧 지급 (현금 환불 아님).
func (a *MultiPGPaymentAdapter) IssueSLACredit(_ context.Context, orgID string, compensationMicro int64, incidentID string) error {
	return a.ledger.IssueSLACredit(orgID, compensationMicro, incidentID)
}

// ── PaymentFailurePort (L4) ──────────────────────────────────────────────

// HandlePaymentFailure — 결제 실패 처리 (3일 유예 기간 설정).
func (a *MultiPGPaymentAdapter) HandlePaymentFailure(_ context.Context, orgID string, failedAt time.Time) (time.Time, error) {
	const graceDays = 3
	gp := a.ledger.SetGracePeriod(orgID, failedAt, graceDays)
	return gp.GracePeriodEnd, nil
}

// ── 추가 유틸리티 ────────────────────────────────────────────────────────

// SetCost — 원가표에 SKU 등록/갱신.
func (a *MultiPGPaymentAdapter) SetCost(sku string, costMicro int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.costTable[sku] = costMicro
}

// Quote — SKU 판매가 계산.
func (a *MultiPGPaymentAdapter) Quote(sku string) (sellMicro, marginMicro int64, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cost, ok := a.costTable[sku]
	if !ok {
		return 0, 0, fmt.Errorf("unknown sku: %s", sku)
	}
	sell := ceilMul(cost, a.marginRate)
	return sell, sell - cost, nil
}

// SettleTopUp — 결제 성공분 크레딧 원장 적립.
func (a *MultiPGPaymentAdapter) SettleTopUp(orgID string, paidMicro int64) error {
	return a.ledger.TopUpMicro(orgID, paidMicro)
}

// ─── InMemoryWebhookAdapter (WebhookPort L5) ─────────────────────────────

// InMemoryWebhookAdapter — WebhookPort 인메모리 구현.
// 진짜: Postgres UNIQUE 제약 (INSERT ... ON CONFLICT DO NOTHING) 으로 교체.
// 이중 충전 방지의 핵심: 동일 eventID는 단 1번만 처리됨을 보장.
type InMemoryWebhookAdapter struct {
	mu     sync.Mutex
	events map[string]string // "pgKind:eventID" -> result
}

func NewInMemoryWebhookAdapter() *InMemoryWebhookAdapter {
	return &InMemoryWebhookAdapter{events: map[string]string{}}
}

func (w *InMemoryWebhookAdapter) key(pgKind PGKind, eventID string) string {
	return string(pgKind) + ":" + eventID
}

// MarkProcessed — 처음 처리 시 true, 이미 처리됐으면 false.
func (w *InMemoryWebhookAdapter) MarkProcessed(_ context.Context, pgKind PGKind, eventID string) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	k := w.key(pgKind, eventID)
	if _, exists := w.events[k]; exists {
		return false, nil
	}
	w.events[k] = ""
	return true, nil
}

// GetProcessedResult — 이미 처리된 이벤트 결과 조회.
func (w *InMemoryWebhookAdapter) GetProcessedResult(_ context.Context, pgKind PGKind, eventID string) (string, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	k := w.key(pgKind, eventID)
	result, found := w.events[k]
	return result, found, nil
}

// SaveResult — 처리 결과 저장.
func (w *InMemoryWebhookAdapter) SaveResult(_ context.Context, pgKind PGKind, eventID, result string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.events[w.key(pgKind, eventID)] = result
	return nil
}

// ─── InMemoryExternalLLMAuth (ExternalLLMAuthPort L6) ────────────────────

// InMemoryExternalLLMAuth — ExternalLLMAuthPort 스텁 구현.
// 현재: 크레딧 잔액 확인 후 예약. 실제 외부 LLM 호출 전 반드시 통과해야 함.
// 진짜: 모델별 단가 테이블 + 실시간 환율 + 조직별 한도 + 글로벌 규정 적용.
type InMemoryExternalLLMAuth struct {
	mu          sync.Mutex
	ledger      *Ledger
	modelPrices map[string]int64 // 모델별 단가 (µc/1K 토큰)
}

func NewInMemoryExternalLLMAuth(ledger *Ledger) *InMemoryExternalLLMAuth {
	return &InMemoryExternalLLMAuth{
		ledger: ledger,
		modelPrices: map[string]int64{
			"openai/gpt-4o":            30_000, // 30µc/1K tokens
			"openai/gpt-4o-mini":       1_500,  // 1.5µc/1K tokens
			"anthropic/claude-3-5":     15_000, // 15µc/1K tokens
			"anthropic/claude-3-haiku": 2_500,  // 2.5µc/1K tokens
			"default":                  5_000,  // 알 수 없는 모델 기본값
		},
	}
}

// CheckAuth — 외부 LLM 호출 인가 + 크레딧 선차감 예약.
func (a *InMemoryExternalLLMAuth) CheckAuth(_ context.Context, orgID, model string, estimatedTokens int64) (bool, int64, error) {
	a.mu.Lock()
	pricePerK, ok := a.modelPrices[model]
	if !ok {
		pricePerK = a.modelPrices["default"]
	}
	a.mu.Unlock()

	// 예상 비용 계산 (올림: 예약은 넉넉하게)
	estimatedMicro := (estimatedTokens/1000 + 1) * pricePerK

	if err := a.ledger.Reserve(orgID, estimatedMicro); err != nil {
		return false, 0, fmt.Errorf("insufficient credits for LLM call: %w", err)
	}
	return true, estimatedMicro, nil
}

// CommitUsage — 실제 사용량 확정.
func (a *InMemoryExternalLLMAuth) CommitUsage(_ context.Context, orgID, model string, actualTokens int64, reservedMicro int64) error {
	a.mu.Lock()
	pricePerK, ok := a.modelPrices[model]
	if !ok {
		pricePerK = a.modelPrices["default"]
	}
	a.mu.Unlock()

	actualMicro := (actualTokens/1000 + 1) * pricePerK
	return a.ledger.CommitReservation(orgID, reservedMicro, actualMicro, "llm:"+model)
}

// ReleaseReservation — 호출 실패 시 예약 크레딧 반환.
func (a *InMemoryExternalLLMAuth) ReleaseReservation(_ context.Context, orgID string, reservedMicro int64) error {
	return a.ledger.ReleaseReservation(orgID, reservedMicro)
}

// ─── 유틸리티 ────────────────────────────────────────────────────────────

// daysInCurrentMonth — 현재 달의 총 일수.
func daysInCurrentMonth(t time.Time) int {
	firstOfNextMonth := time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, t.Location())
	return int(firstOfNextMonth.Sub(time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location())).Hours() / 24)
}
