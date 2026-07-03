package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────
// adapter_consent.go — ConsentPort 구현체
//
// 설계 원칙:
//   - 한국: 가격 인상 30일 전 사전 동의 (전자상거래법 2025).
//   - EU: 약관 변경 시 명시적 재동의 (GDPR).
//   - 미동의 시 갱신 차단 → 구독 자동 취소.
//   - 동의 기록은 불변 감사 로그로 보관.
// ─────────────────────────────────────────────────────────────────────────

// ConsentAdapter — ConsentPort 인메모리 구현체.
type ConsentAdapter struct {
	mu      sync.RWMutex
	records map[string]*ConsentRecord // consentID → record
	bus     EventBusPort
	notif   NotificationPort
}

// 컴파일 타임 인터페이스 계약 검증.
var _ ConsentPort = (*ConsentAdapter)(nil)

// NewConsentAdapter — ConsentAdapter 생성.
func NewConsentAdapter(bus EventBusPort, notif NotificationPort) *ConsentAdapter {
	return &ConsentAdapter{
		records: make(map[string]*ConsentRecord),
		bus:     bus,
		notif:   notif,
	}
}

// RequestConsent — 동의 요청 생성 및 알림 발송.
func (a *ConsentAdapter) RequestConsent(
	ctx context.Context,
	orgID string,
	consentType ConsentType,
	deadline time.Time,
) (string, error) {
	if orgID == "" {
		return "", fmt.Errorf("consent: orgID required")
	}
	if deadline.IsZero() || deadline.Before(time.Now()) {
		return "", fmt.Errorf("consent: deadline must be in the future")
	}

	consentID := newID("consent")
	rec := &ConsentRecord{
		ConsentID: consentID,
		OrgID:     orgID,
		Type:      consentType,
		Status:    ConsentPending,
		Deadline:  deadline,
		CreatedAt: time.Now(),
	}

	a.mu.Lock()
	a.records[consentID] = rec
	a.mu.Unlock()

	// 고객에게 동의 요청 알림 발송.
	if a.notif != nil {
		_ = a.notif.SendTransactional(ctx, NotificationRequest{
			OrgID:    orgID,
			Event:    NotifConsentRequired,
			Channels: []NotificationChannel{ChannelEmail},
			Payload: map[string]string{
				"consent_id":   consentID,
				"consent_type": string(consentType),
				"deadline":     deadline.Format(time.RFC3339),
			},
		})
	}

	// NATS 이벤트 발행.
	_ = a.bus.Publish(ctx, DomainEvent{
		EventID:    newID("evt"),
		Subject:    "sovereign.consent.requested",
		OrgID:      orgID,
		Payload:    []byte(fmt.Sprintf(`{"consent_id":%q,"type":%q,"deadline":%q}`, consentID, consentType, deadline.Format(time.RFC3339))),
		OccurredAt: time.Now(),
	})

	return consentID, nil
}

// RecordConsent — 고객 동의/거부 응답 기록.
func (a *ConsentAdapter) RecordConsent(ctx context.Context, consentID string, accepted bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	rec, ok := a.records[consentID]
	if !ok {
		return fmt.Errorf("consent: not found: %s", consentID)
	}
	if rec.Status != ConsentPending {
		return fmt.Errorf("consent: already responded: %s", rec.Status)
	}
	if time.Now().After(rec.Deadline) {
		rec.Status = ConsentExpired
		return fmt.Errorf("consent: deadline exceeded")
	}

	if accepted {
		rec.Status = ConsentAccepted
	} else {
		rec.Status = ConsentRejected
	}
	rec.RespondedAt = time.Now()

	// NATS 이벤트 발행 (거부 시 구독 취소 봇이 수신).
	subject := "sovereign.consent.accepted"
	if !accepted {
		subject = "sovereign.consent.rejected"
	}
	_ = a.bus.Publish(ctx, DomainEvent{
		EventID:    newID("evt"),
		Subject:    subject,
		OrgID:      rec.OrgID,
		Payload:    []byte(fmt.Sprintf(`{"consent_id":%q,"org_id":%q}`, consentID, rec.OrgID)),
		OccurredAt: time.Now(),
	})

	return nil
}

// GetConsentStatus — 특정 동의 요청 상태 조회.
func (a *ConsentAdapter) GetConsentStatus(ctx context.Context, consentID string) (*ConsentRecord, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	rec, ok := a.records[consentID]
	if !ok {
		return nil, fmt.Errorf("consent: not found: %s", consentID)
	}

	// 기한 초과 시 상태 자동 업데이트 (읽기 시 갱신).
	if rec.Status == ConsentPending && time.Now().After(rec.Deadline) {
		a.mu.RUnlock()
		a.mu.Lock()
		rec.Status = ConsentExpired
		a.mu.Unlock()
		a.mu.RLock()
	}

	// 값 복사 반환 (레이스 방지).
	copy := *rec
	return &copy, nil
}

// ListPendingConsents — 특정 테넌트의 미완료 동의 목록 조회.
func (a *ConsentAdapter) ListPendingConsents(ctx context.Context, orgID string) ([]ConsentRecord, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	var result []ConsentRecord
	for _, rec := range a.records {
		if rec.OrgID != orgID {
			continue
		}
		if rec.Status != ConsentPending {
			continue
		}
		// 기한 초과 필터링.
		if time.Now().After(rec.Deadline) {
			continue
		}
		result = append(result, *rec)
	}
	return result, nil
}
