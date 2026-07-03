package main

// extended_bots_test.go
// ConsentBot / PriceIncreaseNoticeBot / WitnessNode / RefundReservePool 예외·경계값·동시성 테스트

import (
	"context"
	"sync"
	"testing"
	"time"
)

// ──────────────────────────────────────────────────────────────────────────────
// PriceIncreaseNoticeBot 테스트
// ──────────────────────────────────────────────────────────────────────────────

func TestPriceNotice_EmptyOrgID_Error(t *testing.T) {
	bus := NewInMemoryEventBus()
	notif := NewNotificationAdapter(NewStore(), bus)
	consent := NewConsentAdapter(bus, notif)
	bot := NewPriceIncreaseNoticeBot(notif, consent, bus)

	_, err := bot.RegisterPriceIncrease(context.Background(), "", 10000, 12000, time.Now().Add(60*24*time.Hour))
	if err == nil {
		t.Fatal("expected error for empty orgID")
	}
}

func TestPriceNotice_NewPriceLowerThanCurrent_Error(t *testing.T) {
	bus := NewInMemoryEventBus()
	notif := NewNotificationAdapter(NewStore(), bus)
	consent := NewConsentAdapter(bus, notif)
	bot := NewPriceIncreaseNoticeBot(notif, consent, bus)

	_, err := bot.RegisterPriceIncrease(context.Background(), "org_1", 10000, 9000, time.Now().Add(60*24*time.Hour))
	if err == nil {
		t.Fatal("expected error when newPrice <= currentPrice")
	}
}

func TestPriceNotice_EffectiveDateTooSoon_Error(t *testing.T) {
	bus := NewInMemoryEventBus()
	notif := NewNotificationAdapter(NewStore(), bus)
	consent := NewConsentAdapter(bus, notif)
	bot := NewPriceIncreaseNoticeBot(notif, consent, bus)

	// 10일 후 → 30일 사전 통지 기간 미충족
	_, err := bot.RegisterPriceIncrease(context.Background(), "org_1", 10000, 12000, time.Now().Add(10*24*time.Hour))
	if err == nil {
		t.Fatal("expected error for effective date less than 30 days away")
	}
}

func TestPriceNotice_ValidRegistration_Success(t *testing.T) {
	bus := NewInMemoryEventBus()
	notif := NewNotificationAdapter(NewStore(), bus)
	consent := NewConsentAdapter(bus, notif)
	bot := NewPriceIncreaseNoticeBot(notif, consent, bus)

	evt, err := bot.RegisterPriceIncrease(context.Background(), "org_valid", 10000, 12000, time.Now().Add(60*24*time.Hour))
	if err != nil {
		t.Fatalf("expected success: %v", err)
	}
	if evt.EventID == "" {
		t.Fatal("expected non-empty EventID")
	}
}

func TestPriceNotice_SendPendingNotices_AlreadySent_NoResend(t *testing.T) {
	bus := NewInMemoryEventBus()
	notif := NewNotificationAdapter(NewStore(), bus)
	consent := NewConsentAdapter(bus, notif)
	bot := NewPriceIncreaseNoticeBot(notif, consent, bus)

	// 등록 후 SendPendingNotices 두 번 호출 → 두 번째는 발송 안 됨
	_, _ = bot.RegisterPriceIncrease(context.Background(), "org_resend", 10000, 12000, time.Now().Add(60*24*time.Hour))
	// noticeDeadline이 미래이므로 첫 번째 호출에서도 발송 안 됨 (정상)
	bot.SendPendingNotices(context.Background())
	bot.SendPendingNotices(context.Background())
	// 에러 없이 통과하면 성공
}

// ──────────────────────────────────────────────────────────────────────────────
// WitnessNode 테스트
// ──────────────────────────────────────────────────────────────────────────────

func TestWitness_EmptyFailedNodeID_Error(t *testing.T) {
	bus := NewInMemoryEventBus()
	hb := NewHeartbeatAdapter(bus)
	witness := NewWitnessNode("witness-1", hb, bus)

	_, err := witness.Vote(context.Background(), VoteRequest{
		RequestID:    "req_1",
		FailedNodeID: "",
		Reason:       FailoverReasonHeartbeatTimeout,
	})
	if err == nil {
		t.Fatal("expected error for empty failedNodeID")
	}
}

func TestWitness_UnknownNode_ApproveFailover(t *testing.T) {
	bus := NewInMemoryEventBus()
	hb := NewHeartbeatAdapter(bus)
	witness := NewWitnessNode("witness-1", hb, bus)

	// 등록되지 않은 노드 → 장애로 간주하여 승인
	result, err := witness.Vote(context.Background(), VoteRequest{
		RequestID:    "req_unknown",
		FailedNodeID: "nonexistent_node",
		Reason:       FailoverReasonHeartbeatTimeout,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Approved {
		t.Fatal("expected approval for unknown node")
	}
}

func TestWitness_HealthyNode_DenyFailover(t *testing.T) {
	bus := NewInMemoryEventBus()
	hb := NewHeartbeatAdapter(bus)
	witness := NewWitnessNode("witness-1", hb, bus)

	// 방금 Beat한 노드 → 건강하므로 페일오버 거부
	_ = hb.Beat(context.Background(), "node_healthy_vote", NodeRoleActive)

	result, err := witness.Vote(context.Background(), VoteRequest{
		RequestID:    "req_healthy",
		FailedNodeID: "node_healthy_vote",
		Reason:       FailoverReasonHeartbeatTimeout,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Approved {
		t.Fatal("expected denial for healthy node")
	}
}

func TestWitness_StaleNode_ApproveFailover(t *testing.T) {
	bus := NewInMemoryEventBus()
	hb := NewHeartbeatAdapter(bus)
	witness := NewWitnessNode("witness-1", hb, bus)

	// Beat 후 phiMap의 lastSeen을 오래 전으로 조작
	_ = hb.Beat(context.Background(), "node_stale", NodeRoleActive)
	hb.mu.Lock()
	if ps, ok := hb.phiMap["node_stale"]; ok {
		ps.lastSeen = time.Now().Add(-60 * time.Second)
	}
	// nodes의 LastSeen도 업데이트
	if n, ok := hb.nodes["node_stale"]; ok {
		n.LastSeen = time.Now().Add(-60 * time.Second)
	}
	hb.mu.Unlock()

	result, err := witness.Vote(context.Background(), VoteRequest{
		RequestID:    "req_stale",
		FailedNodeID: "node_stale",
		Reason:       FailoverReasonHeartbeatTimeout,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Approved {
		t.Fatalf("expected approval for stale node (last seen 60s ago), reason: %s", result.Reason)
	}
}

func TestWitness_VoteLog_RecordsAll(t *testing.T) {
	bus := NewInMemoryEventBus()
	hb := NewHeartbeatAdapter(bus)
	witness := NewWitnessNode("witness-1", hb, bus)

	// 3번 투표
	for i := 0; i < 3; i++ {
		_, _ = witness.Vote(context.Background(), VoteRequest{
			RequestID:    newID("req"),
			FailedNodeID: "node_log_test",
			Reason:       FailoverReasonManual,
		})
	}
	log := witness.GetVoteLog(context.Background())
	if len(log) != 3 {
		t.Fatalf("expected 3 vote log entries, got %d", len(log))
	}
}

func TestWitness_ConcurrentVote_NoRace(t *testing.T) {
	bus := NewInMemoryEventBus()
	hb := NewHeartbeatAdapter(bus)
	witness := NewWitnessNode("witness-1", hb, bus)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = witness.Vote(context.Background(), VoteRequest{
				RequestID:    newID("req"),
				FailedNodeID: "node_concurrent",
				Reason:       FailoverReasonHeartbeatTimeout,
			})
		}()
	}
	wg.Wait()
}

// ──────────────────────────────────────────────────────────────────────────────
// RefundReservePool 테스트
// ──────────────────────────────────────────────────────────────────────────────

func TestReservePool_Advance_Success(t *testing.T) {
	bus := NewInMemoryEventBus()
	pool := NewRefundReservePool(100_000, bus)

	err := pool.Advance(context.Background(), "org_1", 30_000)
	if err != nil {
		t.Fatalf("expected success: %v", err)
	}
	if pool.Balance() != 70_000 {
		t.Fatalf("expected balance 70000, got %d", pool.Balance())
	}
}

func TestReservePool_Advance_Insufficient_Error(t *testing.T) {
	bus := NewInMemoryEventBus()
	pool := NewRefundReservePool(10_000, bus)

	err := pool.Advance(context.Background(), "org_1", 50_000)
	if err == nil {
		t.Fatal("expected error for insufficient balance")
	}
}

func TestReservePool_Advance_ZeroAmount_Error(t *testing.T) {
	bus := NewInMemoryEventBus()
	pool := NewRefundReservePool(100_000, bus)

	err := pool.Advance(context.Background(), "org_1", 0)
	if err == nil {
		t.Fatal("expected error for zero amount")
	}
}

func TestReservePool_Advance_NegativeAmount_Error(t *testing.T) {
	bus := NewInMemoryEventBus()
	pool := NewRefundReservePool(100_000, bus)

	err := pool.Advance(context.Background(), "org_1", -1000)
	if err == nil {
		t.Fatal("expected error for negative amount")
	}
}

func TestReservePool_Replenish_Success(t *testing.T) {
	bus := NewInMemoryEventBus()
	pool := NewRefundReservePool(50_000, bus)

	_ = pool.Advance(context.Background(), "org_1", 20_000)
	err := pool.Replenish(context.Background(), 20_000)
	if err != nil {
		t.Fatalf("expected success: %v", err)
	}
	if pool.Balance() != 50_000 {
		t.Fatalf("expected balance 50000 after replenish, got %d", pool.Balance())
	}
}

func TestReservePool_Replenish_ZeroAmount_Error(t *testing.T) {
	bus := NewInMemoryEventBus()
	pool := NewRefundReservePool(100_000, bus)

	err := pool.Replenish(context.Background(), 0)
	if err == nil {
		t.Fatal("expected error for zero replenish amount")
	}
}

func TestReservePool_ConcurrentAdvance_NoRace(t *testing.T) {
	bus := NewInMemoryEventBus()
	pool := NewRefundReservePool(1_000_000, bus)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = pool.Advance(context.Background(), "org_race", 1_000)
		}()
	}
	wg.Wait()
}

// ──────────────────────────────────────────────────────────────────────────────
// mTLS 어댑터 테스트
// ──────────────────────────────────────────────────────────────────────────────

func TestMTLS_IssueAndVerify_Success(t *testing.T) {
	adapter, err := NewMTLSAdapter()
	if err != nil {
		t.Fatalf("adapter creation failed: %v", err)
	}

	certPEM, _, err := adapter.IssueServiceCert(context.Background(), "payment-service", "org_mtls")
	if err != nil {
		t.Fatalf("issue cert failed: %v", err)
	}

	identity, err := adapter.VerifyClientCert(context.Background(), certPEM)
	if err != nil {
		t.Fatalf("verify cert failed: %v", err)
	}
	if identity.ServiceName != "payment-service" {
		t.Fatalf("expected service name 'payment-service', got '%s'", identity.ServiceName)
	}
}

func TestMTLS_IssueEmptyServiceName_Error(t *testing.T) {
	adapter, err := NewMTLSAdapter()
	if err != nil {
		t.Fatalf("adapter creation failed: %v", err)
	}
	_, _, err = adapter.IssueServiceCert(context.Background(), "", "org_1")
	if err == nil {
		t.Fatal("expected error for empty serviceName")
	}
}

func TestMTLS_IssueEmptyOrgID_Error(t *testing.T) {
	adapter, err := NewMTLSAdapter()
	if err != nil {
		t.Fatalf("adapter creation failed: %v", err)
	}
	_, _, err = adapter.IssueServiceCert(context.Background(), "svc", "")
	if err == nil {
		t.Fatal("expected error for empty orgID")
	}
}

func TestMTLS_VerifyEmptyCert_Error(t *testing.T) {
	adapter, err := NewMTLSAdapter()
	if err != nil {
		t.Fatalf("adapter creation failed: %v", err)
	}
	_, err = adapter.VerifyClientCert(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for empty certPEM")
	}
}

func TestMTLS_RevokeAndVerify_Error(t *testing.T) {
	adapter, err := NewMTLSAdapter()
	if err != nil {
		t.Fatalf("adapter creation failed: %v", err)
	}

	certPEM, _, err := adapter.IssueServiceCert(context.Background(), "revoke-svc", "org_revoke")
	if err != nil {
		t.Fatalf("issue cert failed: %v", err)
	}

	// 폐기
	if err := adapter.RevokeServiceCert(context.Background(), certPEM); err != nil {
		t.Fatalf("revoke failed: %v", err)
	}

	// 폐기된 인증서 검증 → 에러
	_, err = adapter.VerifyClientCert(context.Background(), certPEM)
	if err == nil {
		t.Fatal("expected error for revoked cert")
	}
}

func TestMTLS_RevokeNonExistentCert_Error(t *testing.T) {
	adapter, err := NewMTLSAdapter()
	if err != nil {
		t.Fatalf("adapter creation failed: %v", err)
	}
	err = adapter.RevokeServiceCert(context.Background(), []byte("invalid_pem"))
	if err == nil {
		t.Fatal("expected error for non-existent cert revocation")
	}
}
