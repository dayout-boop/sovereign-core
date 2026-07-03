package main

import (
	"context"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────
// integration_test.go — 신규 포트/어댑터/봇 통합 테스트
// ─────────────────────────────────────────────────────────────────────────

// ─── PGRouter 테스트 ─────────────────────────────────────────────────────

func TestPGRouter_RouteByRegion(t *testing.T) {
	store := NewStore()
	router := NewPGRouter(store)
	ctx := context.Background()

	cases := []struct {
		region string
		want   PGKind
	}{
		{"ap-northeast-2", PGToss},
		{"eu-west-1", PGStripe},
		{"us-east-1", PGStripe},
		{"ap-southeast-1", PGStripe},
		{"unknown-region", PGStripe}, // 폴백
	}

	for _, c := range cases {
		got, err := router.RouteByRegion(ctx, c.region)
		if err != nil {
			t.Fatalf("RouteByRegion(%q): unexpected error: %v", c.region, err)
		}
		if got != c.want {
			t.Errorf("RouteByRegion(%q) = %q, want %q", c.region, got, c.want)
		}
	}
}

func TestPGRouter_RegisterPG_RuntimeExtension(t *testing.T) {
	store := NewStore()
	router := NewPGRouter(store)
	ctx := context.Background()

	// 신규 리전 등록 — 기존 코드 변경 없이 확장.
	router.RegisterPG("me-south-1", PGKGInicis)

	got, err := router.RouteByRegion(ctx, "me-south-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != PGKGInicis {
		t.Errorf("expected PGKGInicis, got %q", got)
	}
}

func TestPGRouter_RouteByTenant(t *testing.T) {
	app := NewApp()
	ctx := context.Background()

	// 테넌트 생성.
	org, _, err := app.Signup(ctx, "TestCo", "user1")
	if err != nil {
		t.Fatalf("Signup: %v", err)
	}

	// 서울 리전 → Toss.
	got, err := app.pgRouter.RouteByTenant(ctx, org.ID)
	if err != nil {
		t.Fatalf("RouteByTenant: %v", err)
	}
	if got != PGToss {
		t.Errorf("expected PGToss for KR region, got %q", got)
	}
}

// ─── Notification 테스트 ─────────────────────────────────────────────────

func TestNotification_SendTransactional(t *testing.T) {
	bus := NewInMemoryEventBus()
	store := NewStore()
	notif := NewNotificationAdapter(store, bus)
	ctx := context.Background()

	err := notif.SendTransactional(ctx, NotificationRequest{
		OrgID:    "org-test",
		Event:    NotifPaymentSucceeded,
		Channels: []NotificationChannel{ChannelEmail},
		Payload:  map[string]string{"invoice_id": "inv-001"},
	})
	if err != nil {
		t.Fatalf("SendTransactional: %v", err)
	}

	// NATS 이벤트 발행 확인.
	events := bus.PublishedEvents()
	if len(events) == 0 {
		t.Error("expected NATS event to be published")
	}
}

func TestNotification_ScheduleAndCancel(t *testing.T) {
	bus := NewInMemoryEventBus()
	store := NewStore()
	notif := NewNotificationAdapter(store, bus)
	ctx := context.Background()

	// 30일 후 약관 변경 알림 예약.
	schedID, err := notif.ScheduleNotification(ctx, NotificationRequest{
		OrgID:  "org-test",
		Event:  NotifTermsChanging,
		SendAt: time.Now().Add(30 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("ScheduleNotification: %v", err)
	}
	if schedID == "" {
		t.Error("expected non-empty scheduleID")
	}

	// 취소.
	if err := notif.CancelScheduled(ctx, schedID); err != nil {
		t.Fatalf("CancelScheduled: %v", err)
	}
}

// ─── Operator 4-eyes 테스트 ──────────────────────────────────────────────

func TestOperator_FourEyes_Approval(t *testing.T) {
	bus := NewInMemoryEventBus()
	op := NewOperatorAdapter(bus)
	ctx := context.Background()

	// 1차 요청 (환불 = 금액 변경 → 승인 필요).
	reqID, requiresApproval, err := op.RequestAction(ctx, "op-alice", OpActionRefund, "org-001", "SLA 위반 환불", 5_000_000)
	if err != nil {
		t.Fatalf("RequestAction: %v", err)
	}
	if !requiresApproval {
		t.Error("expected requiresApproval=true for refund action")
	}

	// 자기 승인 금지 검증.
	if err := op.ApproveAction(ctx, "op-alice", reqID); err == nil {
		t.Error("expected error for self-approval, got nil")
	}

	// 2차 시니어 승인.
	if err := op.ApproveAction(ctx, "op-bob", reqID); err != nil {
		t.Fatalf("ApproveAction: %v", err)
	}
}

func TestOperator_FourEyes_Rejection(t *testing.T) {
	bus := NewInMemoryEventBus()
	op := NewOperatorAdapter(bus)
	ctx := context.Background()

	reqID, _, err := op.RequestAction(ctx, "op-alice", OpActionCreditAdjust, "org-002", "보상 크레딧", 1_000_000)
	if err != nil {
		t.Fatalf("RequestAction: %v", err)
	}

	if err := op.RejectAction(ctx, "op-bob", reqID, "사유 불충분"); err != nil {
		t.Fatalf("RejectAction: %v", err)
	}

	// 거부 후 재승인 불가.
	if err := op.ApproveAction(ctx, "op-charlie", reqID); err == nil {
		t.Error("expected error approving rejected request")
	}
}

func TestOperator_AuditLog(t *testing.T) {
	bus := NewInMemoryEventBus()
	op := NewOperatorAdapter(bus)
	ctx := context.Background()

	_, _, _ = op.RequestAction(ctx, "op-alice", OpActionSuspendTenant, "org-003", "약관 위반", 0)

	logs, err := op.GetAuditLog(ctx, "org-003", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("GetAuditLog: %v", err)
	}
	if len(logs) == 0 {
		t.Error("expected audit log entries")
	}
}

// ─── Consent 테스트 ──────────────────────────────────────────────────────

func TestConsent_RequestAndAccept(t *testing.T) {
	bus := NewInMemoryEventBus()
	notif := NewNotificationAdapter(NewStore(), bus)
	consent := NewConsentAdapter(bus, notif)
	ctx := context.Background()

	// 30일 후 기한으로 동의 요청.
	consentID, err := consent.RequestConsent(ctx, "org-001", ConsentPriceIncrease, time.Now().Add(30*24*time.Hour))
	if err != nil {
		t.Fatalf("RequestConsent: %v", err)
	}

	// 동의 수락.
	if err := consent.RecordConsent(ctx, consentID, true); err != nil {
		t.Fatalf("RecordConsent: %v", err)
	}

	rec, err := consent.GetConsentStatus(ctx, consentID)
	if err != nil {
		t.Fatalf("GetConsentStatus: %v", err)
	}
	if rec.Status != ConsentAccepted {
		t.Errorf("expected ConsentAccepted, got %q", rec.Status)
	}
}

func TestConsent_Rejection_BlocksRenewal(t *testing.T) {
	bus := NewInMemoryEventBus()
	consent := NewConsentAdapter(bus, nil)
	ctx := context.Background()

	consentID, _ := consent.RequestConsent(ctx, "org-002", ConsentTermsChange, time.Now().Add(14*24*time.Hour))
	_ = consent.RecordConsent(ctx, consentID, false)

	rec, _ := consent.GetConsentStatus(ctx, consentID)
	if rec.Status != ConsentRejected {
		t.Errorf("expected ConsentRejected, got %q", rec.Status)
	}

	// NATS에 거부 이벤트 발행됐는지 확인.
	events := bus.PublishedEvents()
	found := false
	for _, e := range events {
		if e.Subject == "sovereign.consent.rejected" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected sovereign.consent.rejected event in NATS")
	}
}

// ─── API Key 인증 테스트 ─────────────────────────────────────────────────

func TestAPIKey_IssueAndVerify(t *testing.T) {
	adapter := NewAPIKeyAdapter()
	ctx := context.Background()

	plainKey, info, err := adapter.IssueAPIKey(ctx, "org-001", "test-key", []string{"read", "write"}, time.Time{})
	if err != nil {
		t.Fatalf("IssueAPIKey: %v", err)
	}
	if plainKey == "" || info.KeyID == "" {
		t.Error("expected non-empty key and keyID")
	}

	// 검증.
	verified, err := adapter.VerifyAPIKey(ctx, plainKey)
	if err != nil {
		t.Fatalf("VerifyAPIKey: %v", err)
	}
	if verified.OrgID != "org-001" {
		t.Errorf("expected org-001, got %q", verified.OrgID)
	}
}

func TestAPIKey_HMAC_Verification(t *testing.T) {
	adapter := NewAPIKeyAdapter()
	ctx := context.Background()

	_, info, _ := adapter.IssueAPIKey(ctx, "org-001", "hmac-key", nil, time.Time{})
	payload := []byte(`{"action":"charge","amount":1000}`)
	timestamp := time.Now()

	// 어댑터 내부 키로 HMAC 서명 생성 (테스트 헬퍼).
	sig := adapter.signForTest(info.KeyID, payload)

	if err := adapter.VerifyHMAC(ctx, info.KeyID, payload, timestamp, sig); err != nil {
		t.Fatalf("VerifyHMAC: %v", err)
	}
}

func TestAPIKey_HMAC_ReplayAttack(t *testing.T) {
	adapter := NewAPIKeyAdapter()
	ctx := context.Background()

	_, info, _ := adapter.IssueAPIKey(ctx, "org-001", "key", nil, time.Time{})

	// 6분 전 타임스탬프 → 재생 공격으로 거부.
	oldTimestamp := time.Now().Add(-6 * time.Minute)
	err := adapter.VerifyHMAC(ctx, info.KeyID, []byte("payload"), oldTimestamp, "any-sig")
	if err == nil {
		t.Error("expected replay attack rejection, got nil")
	}
}

// ─── HA 테스트 ───────────────────────────────────────────────────────────

func TestHeartbeat_PhiAccrual(t *testing.T) {
	bus := NewInMemoryEventBus()
	hb := NewHeartbeatAdapter(bus)
	ctx := context.Background()

	// 정상 하트비트.
	for i := 0; i < 5; i++ {
		if err := hb.Beat(ctx, "node-a", NodeRoleActive); err != nil {
			t.Fatalf("Beat: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	health, err := hb.GetHealth(ctx, "node-a")
	if err != nil {
		t.Fatalf("GetHealth: %v", err)
	}
	if !health.IsAlive {
		t.Error("expected node-a to be alive")
	}
}

func TestFailover_STONITH_SelfApprovalPrevention(t *testing.T) {
	bus := NewInMemoryEventBus()
	fo := NewFailoverAdapter(bus)
	ctx := context.Background()

	evt, err := fo.TriggerFailover(ctx, "node-a", FailoverReasonHeartbeatTimeout)
	if err != nil {
		t.Fatalf("TriggerFailover: %v", err)
	}
	if !evt.FencingDone {
		t.Error("expected fencing to be done before failover")
	}
	if !fo.IsFenced("node-a") {
		t.Error("expected node-a to be fenced")
	}
}

func TestBackup_SnapshotAndRestore(t *testing.T) {
	bus := NewInMemoryEventBus()
	bk := NewBackupAdapter(bus)
	ctx := context.Background()

	// 스냅샷 생성.
	rec, err := bk.TriggerSnapshot(ctx, BackupTypeSnapshot)
	if err != nil {
		t.Fatalf("TriggerSnapshot: %v", err)
	}
	if rec.BackupID == "" {
		t.Error("expected non-empty backupID")
	}

	// 복원.
	if err := bk.RestoreFromBackup(ctx, rec.BackupID, "node-a"); err != nil {
		t.Fatalf("RestoreFromBackup: %v", err)
	}
}

func TestBackup_PruneOldBackups(t *testing.T) {
	bus := NewInMemoryEventBus()
	bk := NewBackupAdapter(bus)
	ctx := context.Background()

	// 백업 3개 생성.
	for i := 0; i < 3; i++ {
		_, _ = bk.TriggerSnapshot(ctx, BackupTypeSnapshot)
	}

	// 0일 보존 → 전부 삭제.
	deleted, err := bk.PruneOldBackups(ctx, 0)
	if err != nil {
		t.Fatalf("PruneOldBackups: %v", err)
	}
	if deleted != 3 {
		t.Errorf("expected 3 deleted, got %d", deleted)
	}
}

// ─── FailoverBot 통합 테스트 ─────────────────────────────────────────────

func TestFailoverBot_AutoRecovery(t *testing.T) {
	bus := NewInMemoryEventBus()
	hb := NewHeartbeatAdapter(bus)
	fo := NewFailoverAdapter(bus)
	bk := NewBackupAdapter(bus)
	notif := NewNotificationAdapter(NewStore(), bus)
	bot := NewFailoverBot(hb, fo, bk, notif, bus)
	ctx := context.Background()

	// 봇 시작 (장애 감지 핸들러 등록).
	if err := bot.Start(ctx); err != nil {
		t.Fatalf("FailoverBot.Start: %v", err)
	}

	// 스냅샷 미리 생성 (복원 테스트용).
	_, _ = bk.TriggerSnapshot(ctx, BackupTypeSnapshot)

	// WAL 지연 설정 (50MB → 스냅샷 복원 불필요).
	bk.SetWALLag("node-a", 50*1024*1024)

	// 노드 복구.
	if err := bot.RecoverNode(ctx, "node-a"); err != nil {
		t.Fatalf("RecoverNode: %v", err)
	}

	// NATS 복구 이벤트 확인.
	events := bus.PublishedEvents()
	found := false
	for _, e := range events {
		if e.Subject == "sovereign.ha.node.recovered" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected sovereign.ha.node.recovered event")
	}
}

// ─── SettlementBot 테스트 ────────────────────────────────────────────────

func TestSettlementBot_RunSettlement(t *testing.T) {
	bus := NewInMemoryEventBus()
	store := NewStore()
	notif := NewNotificationAdapter(store, bus)
	bot := NewSettlementBot(&MockInvoice{}, notif, bus, store)
	ctx := context.Background()

	// 사용량 추가.
	store.addMetering(MeteringEvent{OrgID: "org-001", Kind: "cu_hours", Value: 10, At: time.Now()})

	periodStart := time.Now().AddDate(0, -1, 0)
	periodEnd := time.Now()
	inv, err := bot.RunSettlement(ctx, "org-001", periodStart, periodEnd)
	if err != nil {
		t.Fatalf("RunSettlement: %v", err)
	}
	if inv == nil {
		t.Fatal("expected invoice, got nil")
	}
	if inv.TotalMicro <= 0 {
		t.Errorf("expected positive total, got %d", inv.TotalMicro)
	}
}
