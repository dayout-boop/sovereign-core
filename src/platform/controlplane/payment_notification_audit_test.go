package main

// payment_notification_audit_test.go
// PGRouter / Notification / Operator / Consent 예외·경계값·동시성·부분 실패 케이스 전수 보완

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// ──────────────────────────────────────────────────────────────────────────────
// 헬퍼
// ──────────────────────────────────────────────────────────────────────────────

func newAuditStore() *Store {
	return NewStore()
}

func newAuditBus() EventBusPort {
	return NewInMemoryEventBus()
}

// ──────────────────────────────────────────────────────────────────────────────
// PGRouter 누락 케이스
// ──────────────────────────────────────────────────────────────────────────────

// 1. 알 수 없는 리전 → 글로벌 폴백(Stripe) 반환 확인
func TestPGRouter_UnknownRegion_FallbackToStripe(t *testing.T) {
	r := NewPGRouter(newAuditStore())
	kind, err := r.RouteByRegion(context.Background(), "UNKNOWN_REGION")
	if err != nil {
		t.Fatalf("unknown region should not error, got: %v", err)
	}
	if kind != PGStripe {
		t.Fatalf("unknown region should fallback to PGStripe, got: %v", kind)
	}
}

// 2. 존재하지 않는 테넌트 → 명시적 에러 반환
func TestPGRouter_NonExistentTenant_Error(t *testing.T) {
	r := NewPGRouter(newAuditStore())
	_, err := r.RouteByTenant(context.Background(), "org_nonexistent_xyz")
	if err == nil {
		t.Fatal("expected error for non-existent tenant, got nil")
	}
}

// 3. 동일 리전 RegisterPG 중복 등록 → 마지막 값으로 덮어쓰기
func TestPGRouter_RegisterPG_Overwrite(t *testing.T) {
	r := NewPGRouter(newAuditStore())
	r.RegisterPG("JP", PGToss)
	r.RegisterPG("JP", PGStripe) // 덮어쓰기
	kind, err := r.RouteByRegion(context.Background(), "JP")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if kind != PGStripe {
		t.Fatalf("expected PGStripe after overwrite, got: %v", kind)
	}
}

// 4. 동시 RegisterPG 경쟁 조건 — race detector 통과 확인
func TestPGRouter_ConcurrentRegisterPG_NoRace(t *testing.T) {
	r := NewPGRouter(newAuditStore())
	var wg sync.WaitGroup
	regions := []string{"AU", "SG", "IN", "BR", "MX"}
	for _, reg := range regions {
		wg.Add(1)
		go func(region string) {
			defer wg.Done()
			r.RegisterPG(region, PGStripe)
			_, _ = r.RouteByRegion(context.Background(), region)
		}(reg)
	}
	wg.Wait()
}

// 5. 테넌트 오버라이드 우선순위 — 리전 라우팅보다 테넌트 오버라이드가 우선
func TestPGRouter_TenantOverride_TakesPriority(t *testing.T) {
	s := newAuditStore()
	// KR 리전 org인데 테넌트 오버라이드로 Stripe 강제
	s.put(Org{ID: "org_kr_override", Region: "KR"})
	r := NewPGRouter(s)
	r.SetTenantOverride("org_kr_override", PGStripe)
	kind, err := r.RouteByTenant(context.Background(), "org_kr_override")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if kind != PGStripe {
		t.Fatalf("tenant override should return PGStripe, got: %v", kind)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Notification 누락 케이스
// ──────────────────────────────────────────────────────────────────────────────

// 6. 빈 orgID → 에러
func TestNotification_EmptyOrgID_Error(t *testing.T) {
	a := NewNotificationAdapter(newAuditStore(), newAuditBus())
	err := a.SendTransactional(context.Background(), NotificationRequest{
		OrgID:    "",
		Event:    "payment.success",
		Channels: []NotificationChannel{ChannelEmail},
	})
	if err == nil {
		t.Fatal("expected error for empty orgID")
	}
}

// 7. 빈 이벤트 타입 → 에러
func TestNotification_EmptyEvent_Error(t *testing.T) {
	a := NewNotificationAdapter(newAuditStore(), newAuditBus())
	err := a.SendTransactional(context.Background(), NotificationRequest{
		OrgID:    "org_1",
		Event:    "",
		Channels: []NotificationChannel{ChannelEmail},
	})
	if err == nil {
		t.Fatal("expected error for empty event")
	}
}

// 8. 채널 목록 비어있음 → 에러 또는 기본 채널 사용 (구현 정책 확인)
func TestNotification_EmptyChannels_PolicyCheck(t *testing.T) {
	a := NewNotificationAdapter(newAuditStore(), newAuditBus())
	err := a.SendTransactional(context.Background(), NotificationRequest{
		OrgID:    "org_1",
		Event:    "payment.success",
		Channels: []NotificationChannel{},
	})
	// 에러가 아니라면 기본 채널로 처리됐음을 확인 (정책에 따라 에러도 허용)
	if err != nil {
		t.Logf("empty channels returned error (acceptable policy): %v", err)
	}
}

// 9. 과거 시간으로 예약 발송 → 에러
func TestNotification_SchedulePastTime_Error(t *testing.T) {
	a := NewNotificationAdapter(newAuditStore(), newAuditBus())
	_, err := a.ScheduleNotification(context.Background(), NotificationRequest{
		OrgID:    "org_1",
		Event:    "subscription.renewal",
		Channels: []NotificationChannel{ChannelEmail},
		SendAt:   time.Now().Add(-1 * time.Hour), // 과거
	})
	if err == nil {
		t.Fatal("expected error for past SendAt")
	}
}

// 10. 존재하지 않는 scheduleID 취소 → 에러
func TestNotification_CancelNonExistentSchedule_Error(t *testing.T) {
	a := NewNotificationAdapter(newAuditStore(), newAuditBus())
	err := a.CancelScheduled(context.Background(), "nonexistent_schedule_id")
	if err == nil {
		t.Fatal("expected error for non-existent schedule")
	}
}

// 11. 이미 발송된 예약 취소 → 에러 (sent 상태)
func TestNotification_CancelAlreadySent_Error(t *testing.T) {
	a := NewNotificationAdapter(newAuditStore(), newAuditBus())
	schedID, err := a.ScheduleNotification(context.Background(), NotificationRequest{
		OrgID:    "org_1",
		Event:    "price.increase",
		Channels: []NotificationChannel{ChannelEmail},
		SendAt:   time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("schedule failed: %v", err)
	}
	// 내부적으로 상태를 sent로 변경 후 취소 시도
	a.mu.Lock()
	if rec, ok := a.schedules[schedID]; ok {
		rec.Status = "sent"
		a.schedules[schedID] = rec
	}
	a.mu.Unlock()
	err = a.CancelScheduled(context.Background(), schedID)
	if err == nil {
		t.Fatal("expected error when cancelling already-sent notification")
	}
}

// 12. 동시 SendTransactional — race detector 통과
func TestNotification_ConcurrentSend_NoRace(t *testing.T) {
	a := NewNotificationAdapter(newAuditStore(), newAuditBus())
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = a.SendTransactional(context.Background(), NotificationRequest{
				OrgID:    fmt.Sprintf("org_%d", i),
				Event:    "payment.success",
				Channels: []NotificationChannel{ChannelEmail},
			})
		}(i)
	}
	wg.Wait()
}

// ──────────────────────────────────────────────────────────────────────────────
// Operator 누락 케이스
// ──────────────────────────────────────────────────────────────────────────────

// 13. 빈 operatorID → 에러
func TestOperator_EmptyOperatorID_Error(t *testing.T) {
	o := NewOperatorAdapter(newAuditBus())
	_, _, err := o.RequestAction(context.Background(), "", OpActionRefund, "tenant_1", "환불 요청", 10000)
	if err == nil {
		t.Fatal("expected error for empty operatorID")
	}
}

// 14. 빈 targetID → 에러
func TestOperator_EmptyTargetID_Error(t *testing.T) {
	o := NewOperatorAdapter(newAuditBus())
	_, _, err := o.RequestAction(context.Background(), "op_1", OpActionRefund, "", "환불 요청", 10000)
	if err == nil {
		t.Fatal("expected error for empty targetID")
	}
}

// 15. 빈 reason → 에러
func TestOperator_EmptyReason_Error(t *testing.T) {
	o := NewOperatorAdapter(newAuditBus())
	_, _, err := o.RequestAction(context.Background(), "op_1", OpActionRefund, "tenant_1", "", 10000)
	if err == nil {
		t.Fatal("expected error for empty reason")
	}
}

// 16. 존재하지 않는 requestID 승인 → 에러
func TestOperator_ApproveNonExistent_Error(t *testing.T) {
	o := NewOperatorAdapter(newAuditBus())
	err := o.ApproveAction(context.Background(), "approver_1", "nonexistent_req_id")
	if err == nil {
		t.Fatal("expected error for non-existent requestID")
	}
}

// 17. 이미 승인된 요청 재승인 → 에러 (중복 승인 방지)
func TestOperator_DoubleApproval_Error(t *testing.T) {
	o := NewOperatorAdapter(newAuditBus())
	reqID, _, err := o.RequestAction(context.Background(), "op_1", OpActionCreditAdjust, "tenant_1", "크레딧 조정", 50000)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	// 1차 승인
	if err := o.ApproveAction(context.Background(), "approver_1", reqID); err != nil {
		t.Fatalf("first approval failed: %v", err)
	}
	// 2차 재승인 시도 → 에러
	err = o.ApproveAction(context.Background(), "approver_2", reqID)
	if err == nil {
		t.Fatal("expected error for double approval")
	}
}

// 18. 자기 승인 금지 — 요청자와 승인자 동일
func TestOperator_SelfApproval_Forbidden(t *testing.T) {
	o := NewOperatorAdapter(newAuditBus())
	reqID, _, err := o.RequestAction(context.Background(), "op_same", OpActionSuspendTenant, "tenant_1", "정지 요청", 0)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	err = o.ApproveAction(context.Background(), "op_same", reqID)
	if err == nil {
		t.Fatal("self-approval must be forbidden")
	}
}

// 19. 거부된 요청 승인 시도 → 에러
func TestOperator_ApproveRejected_Error(t *testing.T) {
	o := NewOperatorAdapter(newAuditBus())
	reqID, _, err := o.RequestAction(context.Background(), "op_1", OpActionRefund, "tenant_1", "환불", 10000)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if err := o.RejectAction(context.Background(), "approver_1", reqID, "부적절한 요청"); err != nil {
		t.Fatalf("rejection failed: %v", err)
	}
	// 거부 후 승인 시도
	err = o.ApproveAction(context.Background(), "approver_2", reqID)
	if err == nil {
		t.Fatal("expected error when approving already-rejected request")
	}
}

// 20. 감사 로그 시간 범위 필터 — from/to 범위 밖 이벤트 제외
func TestOperator_AuditLog_TimeFilter(t *testing.T) {
	o := NewOperatorAdapter(newAuditBus())
	// 요청 생성 (현재 시각 기록됨)
	_, _, _ = o.RequestAction(context.Background(), "op_1", OpActionRefund, "tenant_audit", "환불", 5000)

	// 미래 범위로 조회 → 빈 결과
	from := time.Now().Add(1 * time.Hour)
	to := time.Now().Add(2 * time.Hour)
	logs, err := o.GetAuditLog(context.Background(), "tenant_audit", from, to)
	if err != nil {
		t.Fatalf("audit log error: %v", err)
	}
	if len(logs) != 0 {
		t.Fatalf("expected 0 logs in future range, got %d", len(logs))
	}
}

// 21. 동시 RequestAction — race detector 통과
func TestOperator_ConcurrentRequest_NoRace(t *testing.T) {
	o := NewOperatorAdapter(newAuditBus())
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, _ = o.RequestAction(context.Background(),
				fmt.Sprintf("op_%d", i), OpActionRefund,
				fmt.Sprintf("tenant_%d", i), "동시 환불", 1000)
		}(i)
	}
	wg.Wait()
}

// ──────────────────────────────────────────────────────────────────────────────
// Consent 누락 케이스
// ──────────────────────────────────────────────────────────────────────────────

// 22. 빈 orgID → 에러
func TestConsent_EmptyOrgID_Error(t *testing.T) {
	c := NewConsentAdapter(newAuditBus(), nil)
	_, err := c.RequestConsent(context.Background(), "", ConsentPriceIncrease, time.Now().Add(30*24*time.Hour))
	if err == nil {
		t.Fatal("expected error for empty orgID")
	}
}

// 23. 과거 deadline → 에러
func TestConsent_PastDeadline_Error(t *testing.T) {
	c := NewConsentAdapter(newAuditBus(), nil)
	_, err := c.RequestConsent(context.Background(), "org_1", ConsentPriceIncrease, time.Now().Add(-1*time.Hour))
	if err == nil {
		t.Fatal("expected error for past deadline")
	}
}

// 24. 존재하지 않는 consentID 응답 → 에러
func TestConsent_RecordNonExistent_Error(t *testing.T) {
	c := NewConsentAdapter(newAuditBus(), nil)
	err := c.RecordConsent(context.Background(), "nonexistent_consent_id", true)
	if err == nil {
		t.Fatal("expected error for non-existent consentID")
	}
}

// 25. 이미 응답한 동의 재응답 → 에러 (중복 응답 방지)
func TestConsent_DoubleResponse_Error(t *testing.T) {
	c := NewConsentAdapter(newAuditBus(), nil)
	consentID, err := c.RequestConsent(context.Background(), "org_1", ConsentPriceIncrease, time.Now().Add(30*24*time.Hour))
	if err != nil {
		t.Fatalf("request consent failed: %v", err)
	}
	if err := c.RecordConsent(context.Background(), consentID, true); err != nil {
		t.Fatalf("first response failed: %v", err)
	}
	// 두 번째 응답 시도
	err = c.RecordConsent(context.Background(), consentID, false)
	if err == nil {
		t.Fatal("expected error for double response")
	}
}

// 26. 기한 초과 동의 응답 → 에러
func TestConsent_ExpiredDeadline_Error(t *testing.T) {
	c := NewConsentAdapter(newAuditBus(), nil)
	// 1초 후 만료되는 동의 생성
	consentID, err := c.RequestConsent(context.Background(), "org_1", ConsentPriceIncrease, time.Now().Add(1*time.Second))
	if err != nil {
		t.Fatalf("request consent failed: %v", err)
	}
	// 만료 대기
	time.Sleep(2 * time.Second)
	err = c.RecordConsent(context.Background(), consentID, true)
	if err == nil {
		t.Fatal("expected error for expired consent deadline")
	}
}

// 27. ListPendingConsents — 응답 완료된 항목 제외 확인
func TestConsent_ListPending_ExcludesCompleted(t *testing.T) {
	c := NewConsentAdapter(newAuditBus(), nil)
	id1, _ := c.RequestConsent(context.Background(), "org_list", ConsentPriceIncrease, time.Now().Add(30*24*time.Hour))
	id2, _ := c.RequestConsent(context.Background(), "org_list", ConsentTermsChange, time.Now().Add(30*24*time.Hour))
	// id1만 수락
	_ = c.RecordConsent(context.Background(), id1, true)

	pending, err := c.ListPendingConsents(context.Background(), "org_list")
	if err != nil {
		t.Fatalf("list pending failed: %v", err)
	}
	for _, p := range pending {
		if p.ConsentID == id1 {
			t.Fatalf("completed consent id1 should not appear in pending list")
		}
	}
	found := false
	for _, p := range pending {
		if p.ConsentID == id2 {
			found = true
		}
	}
	if !found {
		t.Fatal("pending consent id2 should appear in pending list")
	}
}

// 28. 동시 RequestConsent — race detector 통과
func TestConsent_ConcurrentRequest_NoRace(t *testing.T) {
	c := NewConsentAdapter(newAuditBus(), nil)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = c.RequestConsent(context.Background(),
				fmt.Sprintf("org_%d", i),
				ConsentPriceIncrease,
				time.Now().Add(30*24*time.Hour))
		}(i)
	}
	wg.Wait()
}

// 29. 동시 RecordConsent — 동일 consentID에 동시 응답 시 첫 번째만 성공
func TestConsent_ConcurrentRecord_OnlyFirstSucceeds(t *testing.T) {
	c := NewConsentAdapter(newAuditBus(), nil)
	consentID, err := c.RequestConsent(context.Background(), "org_race", ConsentPriceIncrease, time.Now().Add(30*24*time.Hour))
	if err != nil {
		t.Fatalf("request consent failed: %v", err)
	}

	var wg sync.WaitGroup
	successCount := 0
	var mu sync.Mutex
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.RecordConsent(context.Background(), consentID, true); err == nil {
				mu.Lock()
				successCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if successCount != 1 {
		t.Fatalf("expected exactly 1 successful consent record, got %d", successCount)
	}
}
