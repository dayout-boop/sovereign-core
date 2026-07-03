package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────
// bots_extended.go — 추가 자동화 봇 구현
//
// 포함:
//   - ConsentBot: 기한 초과 동의 자동 구독 차단
//   - PriceIncreaseNoticeBot: 가격 인상 30일 전 자동 사전 통지
//   - WitnessNode: Split-Brain 방지 투표 노드
//   - RefundReservePool: 환불 예비금 풀 선지급 처리 (SettlementBot 고도화)
// ─────────────────────────────────────────────────────────────────────────

// ─── ConsentBot ───────────────────────────────────────────────────────────

// ConsentBot — 기한 초과 동의 자동 구독 차단 봇.
// 설계 원칙:
//   - 주기적 스캔으로 동의 기한 초과 테넌트 감지.
//   - 기한 초과 시 구독을 자동 일시 정지하고 갱신 요청 알림 발송.
//   - 동의 완료 시 자동 복원.
type ConsentBot struct {
	consent  ConsentPort
	notif    NotificationPort
	bus      EventBusPort
	store    *Store
	interval time.Duration
	mu       sync.Mutex
	running  bool
}

// NewConsentBot — ConsentBot 생성.
func NewConsentBot(consent ConsentPort, notif NotificationPort, bus EventBusPort, store *Store) *ConsentBot {
	return &ConsentBot{
		consent:  consent,
		notif:    notif,
		bus:      bus,
		store:    store,
		interval: 1 * time.Hour, // 기본 1시간 주기 스캔
	}
}

// Run — ConsentBot 실행 루프 (고루틴으로 실행).
func (b *ConsentBot) Run(ctx context.Context) {
	b.mu.Lock()
	if b.running {
		b.mu.Unlock()
		return
	}
	b.running = true
	b.mu.Unlock()

	ticker := time.NewTicker(b.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			b.mu.Lock()
			b.running = false
			b.mu.Unlock()
			return
		case <-ticker.C:
			b.scanAndBlock(ctx)
		}
	}
}

// scanAndBlock — 동의 기한 초과 테넌트 스캔 및 구독 차단.
// Store에서 모든 Org를 순회하여 미완료 동의 기한 초과 여부 확인.
func (b *ConsentBot) scanAndBlock(ctx context.Context) {
	b.store.mu.RLock()
	orgIDs := make([]string, 0, len(b.store.orgs))
	for id := range b.store.orgs {
		orgIDs = append(orgIDs, id)
	}
	b.store.mu.RUnlock()

	for _, orgID := range orgIDs {
		pending, err := b.consent.ListPendingConsents(ctx, orgID)
		if err != nil {
			continue
		}
		for _, rec := range pending {
			if rec.Status == ConsentPending && time.Now().After(rec.Deadline) {
				// 구독 일시 정지 이벤트 발행.
				_ = b.bus.Publish(ctx, DomainEvent{
					EventID:    newID("evt"),
					Subject:    "sovereign.consent.subscription.suspended",
					OrgID:      orgID,
					Payload:    []byte(fmt.Sprintf(`{"org_id":%q,"consent_id":%q,"reason":"consent_expired"}`, orgID, rec.ConsentID)),
					OccurredAt: time.Now(),
				})
				// 동의 갱신 요청 알림 발송.
				_ = b.notif.SendTransactional(ctx, NotificationRequest{
					OrgID: orgID,
Event: NotifConsentRequired,
				Channels: []NotificationChannel{ChannelEmail},
					Payload: map[string]string{
						"consent_id": rec.ConsentID,
						"reason":     "consent_expired",
					},
				})
			}
		}
	}
}

// RunOnce — 테스트용: 단일 스캔 실행.
func (b *ConsentBot) RunOnce(ctx context.Context) {
	b.scanAndBlock(ctx)
}

// ─── PriceIncreaseNoticeBot ───────────────────────────────────────────────

// PriceIncreaseEvent — 가격 인상 예정 이벤트.
type PriceIncreaseEvent struct {
	EventID        string    `json:"event_id"`
	OrgID          string    `json:"org_id"`
	CurrentPrice   int64     `json:"current_price"`   // 현재 가격 (원 단위)
	NewPrice       int64     `json:"new_price"`        // 인상 후 가격
	EffectiveDate  time.Time `json:"effective_date"`  // 적용 일자
	NoticeDeadline time.Time `json:"notice_deadline"` // 사전 통지 마감 (적용 30일 전)
	NoticeSentAt   time.Time `json:"notice_sent_at,omitempty"`
}

// PriceIncreaseNoticeBot — 가격 인상 30일 전 자동 사전 통지 봇.
// 설계 원칙:
//   - 가격 인상 이벤트 등록 시 30일 전 통지 스케줄 자동 생성.
//   - 통지 발송 후 동의 수집 프로세스 자동 시작.
//   - EU 규정: 14일 철회권 보장 안내 포함.
type PriceIncreaseNoticeBot struct {
	mu      sync.RWMutex
	events  map[string]*PriceIncreaseEvent // eventID → event
	notif   NotificationPort
	consent ConsentPort
	bus     EventBusPort
}

// NewPriceIncreaseNoticeBot — PriceIncreaseNoticeBot 생성.
func NewPriceIncreaseNoticeBot(notif NotificationPort, consent ConsentPort, bus EventBusPort) *PriceIncreaseNoticeBot {
	return &PriceIncreaseNoticeBot{
		events:  make(map[string]*PriceIncreaseEvent),
		notif:   notif,
		consent: consent,
		bus:     bus,
	}
}

// RegisterPriceIncrease — 가격 인상 이벤트 등록.
// 적용 일자가 30일 미만이면 에러 (사전 통지 기간 미충족).
func (b *PriceIncreaseNoticeBot) RegisterPriceIncrease(ctx context.Context, orgID string, currentPrice, newPrice int64, effectiveDate time.Time) (*PriceIncreaseEvent, error) {
	if orgID == "" {
		return nil, fmt.Errorf("price_notice: orgID is required")
	}
	if newPrice <= currentPrice {
		return nil, fmt.Errorf("price_notice: newPrice must be greater than currentPrice")
	}
	noticeDeadline := effectiveDate.Add(-30 * 24 * time.Hour)
	if time.Now().After(noticeDeadline) {
		return nil, fmt.Errorf("price_notice: effective date must be at least 30 days in the future (notice deadline: %v)", noticeDeadline)
	}

	evt := &PriceIncreaseEvent{
		EventID:        newID("pi"),
		OrgID:          orgID,
		CurrentPrice:   currentPrice,
		NewPrice:       newPrice,
		EffectiveDate:  effectiveDate,
		NoticeDeadline: noticeDeadline,
	}

	b.mu.Lock()
	b.events[evt.EventID] = evt
	b.mu.Unlock()

	return evt, nil
}

// SendPendingNotices — 통지 발송 대기 중인 이벤트 처리 (주기적 실행).
func (b *PriceIncreaseNoticeBot) SendPendingNotices(ctx context.Context) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	for _, evt := range b.events {
		if !evt.NoticeSentAt.IsZero() {
			continue // 이미 발송됨
		}
		if now.Before(evt.NoticeDeadline) {
			continue // 아직 발송 시점 아님
		}
		// 30일 전 통지 발송.
		_ = b.notif.SendTransactional(ctx, NotificationRequest{
			OrgID: evt.OrgID,
			Event: NotifTermsChanging,
			Channels: []NotificationChannel{ChannelEmail},
			Payload: map[string]string{
				"current_price":  fmt.Sprintf("%d", evt.CurrentPrice),
				"new_price":      fmt.Sprintf("%d", evt.NewPrice),
				"effective_date": evt.EffectiveDate.Format("2006-01-02"),
				"eu_withdrawal":  "14일 이내 철회 가능",
			},
		})
		// 동의 수집 시작 (30일 후 적용 → deadline = effectiveDate - 1일).
		deadline := evt.EffectiveDate.Add(-24 * time.Hour)
		_, _ = b.consent.RequestConsent(ctx, evt.OrgID, ConsentPriceIncrease, deadline)

		// NATS 이벤트 발행.
		_ = b.bus.Publish(ctx, DomainEvent{
			EventID:    newID("evt"),
			Subject:    "sovereign.billing.price_increase.notice_sent",
			OrgID:      evt.OrgID,
			Payload:    []byte(fmt.Sprintf(`{"event_id":%q,"new_price":%d,"effective_date":%q}`, evt.EventID, evt.NewPrice, evt.EffectiveDate.Format(time.RFC3339))),
			OccurredAt: now,
		})
		evt.NoticeSentAt = now
	}
}

// ─── WitnessNode ─────────────────────────────────────────────────────────

// VoteRequest — Witness 투표 요청.
type VoteRequest struct {
	RequestID    string         `json:"request_id"`
	FailedNodeID string         `json:"failed_node_id"`
	Reason       FailoverReason `json:"reason"`
	RequestedAt  time.Time      `json:"requested_at"`
}

// VoteResult — Witness 투표 결과.
type VoteResult struct {
	RequestID string    `json:"request_id"`
	Approved  bool      `json:"approved"`
	VotedAt   time.Time `json:"voted_at"`
	Reason    string    `json:"reason"`
}

// WitnessNode — Split-Brain 방지 투표 노드.
// 설계 원칙:
//   - 데이터를 저장하지 않고 투표권만 행사.
//   - 2노드 클러스터에서 Quorum(정족수) 확보.
//   - 네트워크 분리 시 양쪽이 동시에 Active가 되는 Split-Brain 방지.
//   - 투표 기준: 요청 노드의 Phi 점수 및 마지막 Heartbeat 시간.
type WitnessNode struct {
	mu        sync.RWMutex
	nodeID    string
	heartbeat HeartbeatPort
	bus       EventBusPort
	voteLog   []VoteResult // 투표 이력 (감사 추적)
}

// NewWitnessNode — WitnessNode 생성.
func NewWitnessNode(nodeID string, heartbeat HeartbeatPort, bus EventBusPort) *WitnessNode {
	return &WitnessNode{
		nodeID:    nodeID,
		heartbeat: heartbeat,
		bus:       bus,
	}
}

// Vote — 페일오버 요청에 대한 투표.
// 승인 기준: failedNode의 마지막 Heartbeat가 30초 이상 경과 또는 Phi > 8.0.
func (w *WitnessNode) Vote(ctx context.Context, req VoteRequest) (*VoteResult, error) {
	if req.FailedNodeID == "" {
		return nil, fmt.Errorf("witness: failedNodeID is required")
	}

	health, err := w.heartbeat.GetHealth(ctx, req.FailedNodeID)
	if err != nil {
		// 노드 정보 없음 → 장애로 간주하여 승인.
		result := &VoteResult{
			RequestID: req.RequestID,
			Approved:  true,
			VotedAt:   time.Now(),
			Reason:    "node_not_found_assumed_failed",
		}
		w.recordVote(ctx, result)
		return result, nil
	}

	// Phi 점수 > 8.0 또는 마지막 Heartbeat 30초 이상 경과 → 승인.
	sinceLastBeat := time.Since(health.LastSeen)
	approved := health.PhiScore > 8.0 || sinceLastBeat > 30*time.Second

	reason := "node_healthy"
	if approved {
		if health.PhiScore > 8.0 {
			reason = fmt.Sprintf("phi_score_exceeded:%.2f", health.PhiScore)
		} else {
			reason = fmt.Sprintf("heartbeat_timeout:%v", sinceLastBeat.Round(time.Second))
		}
	}

	result := &VoteResult{
		RequestID: req.RequestID,
		Approved:  approved,
		VotedAt:   time.Now(),
		Reason:    reason,
	}
	w.recordVote(ctx, result)
	return result, nil
}

// recordVote — 투표 이력 기록 및 NATS 이벤트 발행.
func (w *WitnessNode) recordVote(ctx context.Context, result *VoteResult) {
	w.mu.Lock()
	w.voteLog = append(w.voteLog, *result)
	w.mu.Unlock()

	_ = w.bus.Publish(ctx, DomainEvent{
		EventID:    newID("evt"),
		Subject:    "sovereign.ha.witness.voted",
		OrgID:      "",
		Payload:    []byte(fmt.Sprintf(`{"request_id":%q,"approved":%v,"reason":%q}`, result.RequestID, result.Approved, result.Reason)),
		OccurredAt: result.VotedAt,
	})
}

// GetVoteLog — 투표 이력 조회.
func (w *WitnessNode) GetVoteLog(ctx context.Context) []VoteResult {
	w.mu.RLock()
	defer w.mu.RUnlock()
	result := make([]VoteResult, len(w.voteLog))
	copy(result, w.voteLog)
	return result
}

// ─── RefundReservePool (SettlementBot 고도화) ─────────────────────────────

// RefundReservePool — 환불 예비금 풀.
// 설계 원칙:
//   - 환불 요청 시 예비금 풀에서 선지급 후 PG사 환불 완료 시 정산.
//   - 예비금 풀 잔액 부족 시 NATS 경고 이벤트 발행.
//   - 잔액 20% 미만 시 자동 경고 알림.
type RefundReservePool struct {
	mu           sync.Mutex
	balanceCents int64 // 예비금 잔액 (센트 단위)
	bus          EventBusPort
}

// NewRefundReservePool — RefundReservePool 생성.
func NewRefundReservePool(initialBalanceCents int64, bus EventBusPort) *RefundReservePool {
	return &RefundReservePool{
		balanceCents: initialBalanceCents,
		bus:          bus,
	}
}

// Advance — 환불 선지급 처리.
// 잔액 부족 시 에러 반환 및 NATS 경고 이벤트 발행.
func (p *RefundReservePool) Advance(ctx context.Context, orgID string, amountCents int64) error {
	if amountCents <= 0 {
		return fmt.Errorf("reserve_pool: amountCents must be > 0, got %d", amountCents)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.balanceCents < amountCents {
		_ = p.bus.Publish(ctx, DomainEvent{
			EventID:    newID("evt"),
			Subject:    "sovereign.settlement.reserve_pool.insufficient",
			OrgID:      orgID,
			Payload:    []byte(fmt.Sprintf(`{"balance":%d,"requested":%d}`, p.balanceCents, amountCents)),
			OccurredAt: time.Now(),
		})
		return fmt.Errorf("reserve_pool: insufficient balance (%d cents available, %d cents requested)", p.balanceCents, amountCents)
	}
	p.balanceCents -= amountCents
	// 잔액 20% 미만 시 경고.
	threshold := p.balanceCents + amountCents // 차감 전 원래 잔액 기준
	if p.balanceCents < threshold/5 {
		_ = p.bus.Publish(ctx, DomainEvent{
			EventID:    newID("evt"),
			Subject:    "sovereign.settlement.reserve_pool.low_balance",
			OrgID:      orgID,
			Payload:    []byte(fmt.Sprintf(`{"balance":%d}`, p.balanceCents)),
			OccurredAt: time.Now(),
		})
	}
	return nil
}

// Replenish — PG사 환불 완료 후 예비금 풀 보충.
func (p *RefundReservePool) Replenish(ctx context.Context, amountCents int64) error {
	if amountCents <= 0 {
		return fmt.Errorf("reserve_pool: amountCents must be > 0, got %d", amountCents)
	}
	p.mu.Lock()
	p.balanceCents += amountCents
	p.mu.Unlock()
	return nil
}

// Balance — 현재 예비금 잔액 조회.
func (p *RefundReservePool) Balance() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.balanceCents
}
