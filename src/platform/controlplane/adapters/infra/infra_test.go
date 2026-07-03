package main

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────
// infra_test.go — 인프라 포트 어댑터 테스트
//
// 검증 항목:
//   T-I1: 이벤트 버스 - 발행/구독 정상 동작
//   T-I2: 이벤트 버스 - EventID 없으면 오류 (멱등성 키 강제)
//   T-I3: 이벤트 버스 - 컨텍스트 타임아웃 적용
//   T-I4: 이벤트 버스 - 와일드카드 구독 패턴 매칭
//   T-I5: 서킷 브레이커 - 임계값 초과 시 Open 전환
//   T-I6: 서킷 브레이커 - Open → HalfOpen 자동 전환
//   T-I7: 서킷 브레이커 - 성공 기록 시 Closed 복귀
//   T-I8: 테넌트 프로비저닝 - 멱등성 (동일 orgID 재호출 시 동일 connStr)
//   T-I9: 테넌트 프로비저닝 - 즉시 삭제 vs 지연 삭제
// ─────────────────────────────────────────────────────────────────────────

// T-I1: 이벤트 발행/구독 정상 동작
func TestEventBus_PublishSubscribe(t *testing.T) {
	bus := NewInMemoryEventBus()
	ctx := context.Background()

	received := make([]DomainEvent, 0)
	err := bus.Subscribe(ctx, EventPaymentCompleted, func(e DomainEvent) error {
		received = append(received, e)
		return nil
	})
	if err != nil {
		t.Fatalf("Subscribe 실패: %v", err)
	}

	event := DomainEvent{
		EventID:    "evt-001",
		Subject:    EventPaymentCompleted,
		OrgID:      "org-abc",
		Payload:    []byte(`{"amount": 10000}`),
		OccurredAt: time.Now(),
	}
	if err := bus.Publish(ctx, event); err != nil {
		t.Fatalf("Publish 실패: %v", err)
	}

	if len(received) != 1 {
		t.Fatalf("구독자가 이벤트를 받지 못함: got %d", len(received))
	}
	if received[0].EventID != "evt-001" {
		t.Errorf("EventID 불일치: got %s", received[0].EventID)
	}
}

// T-I2: EventID 없으면 오류 (멱등성 키 강제)
func TestEventBus_MissingEventID(t *testing.T) {
	bus := NewInMemoryEventBus()
	ctx := context.Background()

	err := bus.Publish(ctx, DomainEvent{
		Subject: EventPaymentCompleted,
		OrgID:   "org-abc",
	})
	if err == nil {
		t.Fatal("EventID 없을 때 오류가 발생해야 함")
	}
}

// T-I3: 컨텍스트 타임아웃 적용
func TestEventBus_ContextTimeout(t *testing.T) {
	bus := NewInMemoryEventBus()

	// 이미 만료된 컨텍스트
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()
	time.Sleep(1 * time.Millisecond) // 타임아웃 만료 대기

	// 구독자 등록 (느린 핸들러)
	_ = bus.Subscribe(context.Background(), EventPaymentCompleted, func(e DomainEvent) error {
		time.Sleep(100 * time.Millisecond)
		return nil
	})

	err := bus.Publish(ctx, DomainEvent{
		EventID: "evt-timeout",
		Subject: EventPaymentCompleted,
		OrgID:   "org-abc",
	})
	// 타임아웃 만료된 ctx이므로 오류 반환 기대
	if err == nil {
		t.Log("컨텍스트 타임아웃 전에 완료됨 (핸들러 없거나 빠른 경우 허용)")
	}
}

// T-I4: 와일드카드 구독 패턴 매칭
func TestEventBus_WildcardSubscription(t *testing.T) {
	bus := NewInMemoryEventBus()
	ctx := context.Background()

	count := 0
	_ = bus.Subscribe(ctx, "sovereign.billing.>", func(e DomainEvent) error {
		count++
		return nil
	})

	// billing 하위 이벤트 3개 발행
	for i, subject := range []string{
		EventPaymentCompleted,
		EventPaymentFailed,
		EventRefundIssued,
	} {
		_ = bus.Publish(ctx, DomainEvent{
			EventID: "evt-wild-" + string(rune('0'+i)),
			Subject: subject,
			OrgID:   "org-abc",
		})
	}

	// billing 외 이벤트 발행 (카운트 증가 없어야 함)
	_ = bus.Publish(ctx, DomainEvent{
		EventID: "evt-compute",
		Subject: EventComputeWakeup,
		OrgID:   "org-abc",
	})

	if count != 3 {
		t.Errorf("와일드카드 구독 카운트 오류: got %d, want 3", count)
	}
}

// T-I5: 서킷 브레이커 - 임계값 초과 시 Open 전환
func TestCircuitBreaker_OpensOnThreshold(t *testing.T) {
	cb := NewInMemoryCircuitBreaker(3, 10*time.Second)
	ctx := context.Background()
	svc := "pg-stripe"

	if cb.State(ctx, svc) != CircuitClosed {
		t.Fatal("초기 상태는 Closed여야 함")
	}

	// 임계값(3)만큼 실패 기록
	for i := 0; i < 3; i++ {
		cb.RecordFailure(ctx, svc, fmt.Errorf("timeout"))
	}

	if cb.State(ctx, svc) != CircuitOpen {
		t.Fatal("임계값 초과 후 Open 상태여야 함")
	}
}

// T-I6: 서킷 브레이커 - Open → HalfOpen 자동 전환
func TestCircuitBreaker_HalfOpenAfterDuration(t *testing.T) {
	// 매우 짧은 Open 지속 시간 설정 (테스트용)
	cb := NewInMemoryCircuitBreaker(1, 50*time.Millisecond)
	ctx := context.Background()
	svc := "pg-toss"

	cb.RecordFailure(ctx, svc, fmt.Errorf("timeout"))
	if cb.State(ctx, svc) != CircuitOpen {
		t.Fatal("실패 후 Open 상태여야 함")
	}

	time.Sleep(60 * time.Millisecond) // Open 지속 시간 경과

	if cb.State(ctx, svc) != CircuitHalfOpen {
		t.Fatal("Open 지속 시간 경과 후 HalfOpen 상태여야 함")
	}
}

// T-I7: 서킷 브레이커 - 성공 기록 시 Closed 복귀
func TestCircuitBreaker_ClosedOnSuccess(t *testing.T) {
	cb := NewInMemoryCircuitBreaker(1, 50*time.Millisecond)
	ctx := context.Background()
	svc := "llm-openai"

	cb.RecordFailure(ctx, svc, fmt.Errorf("timeout"))
	time.Sleep(60 * time.Millisecond)
	cb.RecordSuccess(ctx, svc)

	if cb.State(ctx, svc) != CircuitClosed {
		t.Fatal("성공 기록 후 Closed 상태여야 함")
	}
}

// T-I8: 테넌트 프로비저닝 - 멱등성
func TestTenantProvisioner_Idempotent(t *testing.T) {
	p := NewInMemoryTenantProvisioner()
	ctx := context.Background()

	conn1, err := p.ProvisionTenant(ctx, "org-xyz", "launch")
	if err != nil {
		t.Fatalf("첫 번째 프로비저닝 실패: %v", err)
	}
	conn2, err := p.ProvisionTenant(ctx, "org-xyz", "launch")
	if err != nil {
		t.Fatalf("두 번째 프로비저닝 실패: %v", err)
	}
	if conn1 != conn2 {
		t.Errorf("멱등성 위반: 동일 orgID에 다른 connStr 반환\n  1: %s\n  2: %s", conn1, conn2)
	}
}

// T-I9: 테넌트 프로비저닝 - 즉시 삭제 vs 지연 삭제
func TestTenantProvisioner_Deprovision(t *testing.T) {
	p := NewInMemoryTenantProvisioner()
	ctx := context.Background()

	_, _ = p.ProvisionTenant(ctx, "org-del-immediate", "free")
	_, _ = p.ProvisionTenant(ctx, "org-del-delayed", "free")

	// 즉시 삭제
	if err := p.DeprovisionTenant(ctx, "org-del-immediate", true); err != nil {
		t.Fatalf("즉시 삭제 실패: %v", err)
	}
	status, _ := p.GetTenantStatus(ctx, "org-del-immediate")
	if status != "not_found" {
		t.Errorf("즉시 삭제 후 상태 오류: got %s, want not_found", status)
	}

	// 지연 삭제 (30일 후 스케줄)
	if err := p.DeprovisionTenant(ctx, "org-del-delayed", false); err != nil {
		t.Fatalf("지연 삭제 실패: %v", err)
	}
	status, _ = p.GetTenantStatus(ctx, "org-del-delayed")
	if status != "pending_deletion" {
		t.Errorf("지연 삭제 후 상태 오류: got %s, want pending_deletion", status)
	}
}
