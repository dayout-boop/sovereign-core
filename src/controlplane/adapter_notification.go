package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────
// adapter_notification.go — NotificationPort 구현체
//
// 설계 원칙:
//   - 고객의 가입 이메일 기준으로 NATS 이벤트 발생 시 자동 트리거.
//   - 채널 어댑터(Email/Push/InApp)는 교체 가능한 플러그인 구조.
//   - ScheduleNotification: 예약 발송 (30일 전 약관 변경 고지 등).
//   - 인메모리 구현 → 프로덕션: SMTP/FCM/APNS 어댑터로 교체.
// ─────────────────────────────────────────────────────────────────────────

// deliveryRecord — 발송 기록 (내부 추적용).
type deliveryRecord struct {
	NotificationID string
	OrgID          string
	Event          NotificationEvent
	Channels       []NotificationChannel
	Status         string // "queued" | "sent" | "failed" | "scheduled"
	ScheduleID     string
	SendAt         time.Time
	SentAt         time.Time
}

// NotificationAdapter — NotificationPort 인메모리 구현체.
type NotificationAdapter struct {
	mu        sync.RWMutex
	records   map[string]*deliveryRecord // notificationID → record
	schedules map[string]*deliveryRecord // scheduleID → record
	store     *Store
	bus       EventBusPort
}

// 컴파일 타임 인터페이스 계약 검증.
var _ NotificationPort = (*NotificationAdapter)(nil)

// NewNotificationAdapter — NotificationAdapter 생성.
func NewNotificationAdapter(store *Store, bus EventBusPort) *NotificationAdapter {
	a := &NotificationAdapter{
		records:   make(map[string]*deliveryRecord),
		schedules: make(map[string]*deliveryRecord),
		store:     store,
		bus:       bus,
	}
	return a
}

// SendTransactional — 즉시 발송 (결제 완료, 환불 완료 등).
func (a *NotificationAdapter) SendTransactional(ctx context.Context, req NotificationRequest) error {
	if req.OrgID == "" {
		return fmt.Errorf("notification: orgID required")
	}
	if req.Event == "" {
		return fmt.Errorf("notification: event type required")
	}
	if len(req.Channels) == 0 {
		req.Channels = []NotificationChannel{ChannelEmail} // 기본: 이메일
	}

	notifID := newID("notif")
	rec := &deliveryRecord{
		NotificationID: notifID,
		OrgID:          req.OrgID,
		Event:          req.Event,
		Channels:       req.Channels,
		Status:         "sent",
		SentAt:         time.Now(),
	}

	a.mu.Lock()
	a.records[notifID] = rec
	a.mu.Unlock()

	// NATS 이벤트 발행: 알림 발송 완료 추적.
	_ = a.bus.Publish(ctx, DomainEvent{
		EventID:    newID("evt"),
		Subject:    "sovereign.notification.sent",
		OrgID:      req.OrgID,
		Payload:    []byte(fmt.Sprintf(`{"notification_id":%q,"event":%q}`, notifID, req.Event)),
		OccurredAt: time.Now(),
	})

	return nil
}

// ScheduleNotification — 예약 발송 등록.
func (a *NotificationAdapter) ScheduleNotification(ctx context.Context, req NotificationRequest) (string, error) {
	if req.OrgID == "" {
		return "", fmt.Errorf("notification: orgID required")
	}
	if req.SendAt.IsZero() {
		return "", fmt.Errorf("notification: SendAt required for scheduled notification")
	}
	if req.SendAt.Before(time.Now()) {
		return "", fmt.Errorf("notification: SendAt must be in the future")
	}

	scheduleID := newID("sched")
	notifID := newID("notif")
	rec := &deliveryRecord{
		NotificationID: notifID,
		OrgID:          req.OrgID,
		Event:          req.Event,
		Channels:       req.Channels,
		Status:         "scheduled",
		ScheduleID:     scheduleID,
		SendAt:         req.SendAt,
	}

	a.mu.Lock()
	a.schedules[scheduleID] = rec
	a.records[notifID] = rec
	a.mu.Unlock()

	return scheduleID, nil
}

// CancelScheduled — 예약 발송 취소.
func (a *NotificationAdapter) CancelScheduled(ctx context.Context, scheduleID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	rec, ok := a.schedules[scheduleID]
	if !ok {
		return fmt.Errorf("notification: schedule not found: %s", scheduleID)
	}
	if rec.Status != "scheduled" {
		return fmt.Errorf("notification: cannot cancel schedule in status %q", rec.Status)
	}
	rec.Status = "cancelled"
	return nil
}

// GetDeliveryStatus — 발송 상태 조회.
func (a *NotificationAdapter) GetDeliveryStatus(ctx context.Context, notificationID string) (string, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	rec, ok := a.records[notificationID]
	if !ok {
		return "", fmt.Errorf("notification: not found: %s", notificationID)
	}
	return rec.Status, nil
}
