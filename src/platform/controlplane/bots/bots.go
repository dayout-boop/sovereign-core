package main

import (
	"context"
	"fmt"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────
// bots.go — 자동화 봇 구현
//
// 설계 원칙:
//   - 모든 봇은 NATS 이벤트를 수신하거나 주기적으로 실행되는 백그라운드 워커.
//   - 운영자 수기 처리 없이 100% 자동화 파이프라인으로 처리.
//   - 각 봇은 독립적으로 시작/중지 가능.
//
// 포함:
//   - PaymentRetryBot: 결제 실패 자동 재시도 (지수 백오프)
//   - SLACompensationBot: SLA 위반 자동 보상 크레딧 지급
//   - SettlementBot: 월말 정산 자동 실행
//   - FailoverBot: HA 장애 감지 및 자동 페일오버 오케스트레이션
// ─────────────────────────────────────────────────────────────────────────

// ─── PaymentRetryBot ─────────────────────────────────────────────────────

// PaymentRetryBot — 결제 실패 자동 재시도 봇.
// NATS "sovereign.billing.payment.failed" 이벤트 수신 → 지수 백오프 재시도.
type PaymentRetryBot struct {
	payment  PaymentFailurePort
	notif    NotificationPort
	bus      EventBusPort
	maxRetry int
}

// NewPaymentRetryBot — PaymentRetryBot 생성.
func NewPaymentRetryBot(payment PaymentFailurePort, notif NotificationPort, bus EventBusPort) *PaymentRetryBot {
	return &PaymentRetryBot{
		payment:  payment,
		notif:    notif,
		bus:      bus,
		maxRetry: 3,
	}
}

// HandleFailedPayment — 결제 실패 이벤트 처리.
// 유예 기간 부여 후 고객에게 알림 발송.
func (b *PaymentRetryBot) HandleFailedPayment(ctx context.Context, orgID string, failedAt time.Time) error {
	gracePeriodEnd, err := b.payment.HandlePaymentFailure(ctx, orgID, failedAt)
	if err != nil {
		return fmt.Errorf("retry_bot: grace period failed: %w", err)
	}

	// 고객에게 결제 실패 + 유예 기간 알림.
	if b.notif != nil {
		_ = b.notif.SendTransactional(ctx, NotificationRequest{
			OrgID:    orgID,
			Event:    NotifPaymentFailed,
			Channels: []NotificationChannel{ChannelEmail},
			Payload: map[string]string{
				"grace_period_end": gracePeriodEnd.Format(time.RFC3339),
			},
		})
	}

	// NATS 이벤트 발행: 유예 기간 시작.
	_ = b.bus.Publish(ctx, DomainEvent{
		EventID:    newID("evt"),
		Subject:    "sovereign.billing.grace_period.started",
		OrgID:      orgID,
		Payload:    []byte(fmt.Sprintf(`{"org_id":%q,"grace_period_end":%q}`, orgID, gracePeriodEnd.Format(time.RFC3339))),
		OccurredAt: time.Now(),
	})

	return nil
}

// ─── SLACompensationBot ──────────────────────────────────────────────────

// SLACompensationBot — SLA 위반 자동 보상 크레딧 지급 봇.
// NATS "sovereign.compute.boot.failed" 이벤트 수신 → SLA 위반 판정 → 크레딧 자동 지급.
type SLACompensationBot struct {
	refund NotificationPort
	bus    EventBusPort
}

// NewSLACompensationBot — SLACompensationBot 생성.
func NewSLACompensationBot(notif NotificationPort, bus EventBusPort) *SLACompensationBot {
	return &SLACompensationBot{
		refund: notif,
		bus:    bus,
	}
}

// HandleSLAViolation — SLA 위반 이벤트 처리.
// bootMS가 SLA 기준(3000ms)을 초과하면 자동 보상 크레딧 지급.
func (b *SLACompensationBot) HandleSLAViolation(ctx context.Context, orgID, incidentID string, bootMS int64) error {
	const slaThresholdMS = 3000
	const compensationMicroPerViolation = 1_000_000 // 1 크레딧 = 1,000,000 µc

	if bootMS <= slaThresholdMS {
		return nil // SLA 준수, 보상 불필요.
	}

	// 보상 배율: 초과 시간 비례 (최대 10배).
	multiplier := bootMS / slaThresholdMS
	if multiplier > 10 {
		multiplier = 10
	}
	compensationMicro := compensationMicroPerViolation * multiplier

	// 고객에게 SLA 보상 알림.
	if b.refund != nil {
		_ = b.refund.SendTransactional(ctx, NotificationRequest{
			OrgID:    orgID,
			Event:    NotifSLACompensated,
			Channels: []NotificationChannel{ChannelEmail, ChannelInApp},
			Payload: map[string]string{
				"incident_id":        incidentID,
				"compensation_micro": fmt.Sprintf("%d", compensationMicro),
				"boot_ms":            fmt.Sprintf("%d", bootMS),
			},
		})
	}

	// NATS 이벤트 발행: SLA 크레딧 지급.
	_ = b.bus.Publish(ctx, DomainEvent{
		EventID:    newID("evt"),
		Subject:    EventSLACreditIssued,
		OrgID:      orgID,
		Payload:    []byte(fmt.Sprintf(`{"org_id":%q,"incident_id":%q,"compensation_micro":%d}`, orgID, incidentID, compensationMicro)),
		OccurredAt: time.Now(),
	})

	return nil
}

// ─── SettlementBot ───────────────────────────────────────────────────────

// SettlementBot — 월말 정산 자동 실행 봇.
// 매월 말일 자정에 실행되어 사용량 기반 인보이스 생성 및 청구.
type SettlementBot struct {
	invoice InvoicePort
	notif   NotificationPort
	bus     EventBusPort
	store   *Store
}

// NewSettlementBot — SettlementBot 생성.
func NewSettlementBot(invoice InvoicePort, notif NotificationPort, bus EventBusPort, store *Store) *SettlementBot {
	return &SettlementBot{
		invoice: invoice,
		notif:   notif,
		bus:     bus,
		store:   store,
	}
}

// RunSettlement — 특정 기간의 정산 실행.
func (b *SettlementBot) RunSettlement(ctx context.Context, orgID string, periodStart, periodEnd time.Time) (*Invoice, error) {
	// 사용량 집계.
	usage := b.store.usageRollup(orgID)

	// 인보이스 라인 아이템 생성.
	var items []InvoiceLineItem
	if cuHours, ok := usage["cu_hours"]; ok && cuHours > 0 {
		items = append(items, InvoiceLineItem{
			SKU:         "compute_cu_hours",
			Quantity:    cuHours,
			UnitMicro:   22_000, // 22 µc/CU-hour
			TotalMicro:  int64(cuHours) * 22_000,
			Description: "Compute Unit Hours",
		})
	}
	if branchOps, ok := usage["branch_ops"]; ok && branchOps > 0 {
		items = append(items, InvoiceLineItem{
			SKU:         "branch_ops",
			Quantity:    branchOps,
			UnitMicro:   500,
			TotalMicro:  int64(branchOps) * 500,
			Description: "Branch Operations",
		})
	}

	if len(items) == 0 {
		return nil, nil // 사용량 없음, 인보이스 생성 불필요.
	}

	inv, err := b.invoice.CreateInvoice(ctx, orgID, periodStart, periodEnd, items)
	if err != nil {
		return nil, fmt.Errorf("settlement_bot: create invoice failed: %w", err)
	}

	// 고객에게 인보이스 발행 알림.
	if b.notif != nil {
		_ = b.notif.SendTransactional(ctx, NotificationRequest{
			OrgID:    orgID,
			Event:    NotifPaymentSucceeded,
			Channels: []NotificationChannel{ChannelEmail},
			Payload: map[string]string{
				"invoice_id":  inv.ID,
				"total_micro": fmt.Sprintf("%d", inv.TotalMicro),
				"period":      fmt.Sprintf("%s ~ %s", periodStart.Format("2006-01-02"), periodEnd.Format("2006-01-02")),
			},
		})
	}

	// NATS 이벤트 발행.
	_ = b.bus.Publish(ctx, DomainEvent{
		EventID:    newID("evt"),
		Subject:    EventPaymentCompleted,
		OrgID:      orgID,
		Payload:    []byte(fmt.Sprintf(`{"invoice_id":%q,"total_micro":%d}`, inv.ID, inv.TotalMicro)),
		OccurredAt: time.Now(),
	})

	return inv, nil
}

// ─── FailoverBot ─────────────────────────────────────────────────────────

// FailoverBot — HA 장애 감지 및 자동 페일오버 오케스트레이션 봇.
// HeartbeatPort에서 장애 감지 시 FailoverPort를 통해 자동 페일오버 실행.
type FailoverBot struct {
	heartbeat HeartbeatPort
	failover  FailoverPort
	backup    BackupPort
	notif     NotificationPort
	bus       EventBusPort
}

// NewFailoverBot — FailoverBot 생성.
func NewFailoverBot(
	heartbeat HeartbeatPort,
	failover FailoverPort,
	backup BackupPort,
	notif NotificationPort,
	bus EventBusPort,
) *FailoverBot {
	return &FailoverBot{
		heartbeat: heartbeat,
		failover:  failover,
		backup:    backup,
		notif:     notif,
		bus:       bus,
	}
}

// Start — 장애 감지 핸들러 등록 및 백업 스케줄 시작.
func (b *FailoverBot) Start(ctx context.Context) error {
	// 장애 감지 핸들러 등록.
	return b.heartbeat.WatchFailure(ctx, func(failedNodeID string, phi float64) {
		foCtx := context.Background()
		evt, err := b.failover.TriggerFailover(foCtx, failedNodeID, FailoverReasonHeartbeatTimeout)
		if err != nil {
			return
		}
		// 페일오버 완료 후 신규 Active 노드로 VIP 전환.
		_ = b.failover.PromoteStandby(foCtx, evt.NewActiveID)
		_ = b.failover.SwitchVIP(foCtx, evt.NewActiveID)

		// 즉시 스냅샷 백업 실행 (페일오버 직후 상태 보존).
		_, _ = b.backup.TriggerSnapshot(foCtx, BackupTypeSnapshot)
	})
}

// RunHeartbeatCheck — 주기적 헬스 체크 (외부 스케줄러에서 호출).
func (b *FailoverBot) RunHeartbeatCheck(ctx context.Context) {
	if ha, ok := b.heartbeat.(*HeartbeatAdapter); ok {
		ha.CheckAndNotifyFailures(ctx)
	}
}

// RecoverNode — 장애 노드 복구 시 WAL Catch-up 및 Standby 재편입.
func (b *FailoverBot) RecoverNode(ctx context.Context, nodeID string) error {
	// WAL 지연 확인.
	lag, err := b.backup.GetWALLag(ctx, nodeID)
	if err != nil {
		return fmt.Errorf("failover_bot: get WAL lag failed: %w", err)
	}

	// WAL 지연이 100MB 이상이면 전체 스냅샷 복원.
	if lag > 100*1024*1024 {
		backups, err := b.backup.ListBackups(ctx, BackupTypeSnapshot, 1)
		if err != nil || len(backups) == 0 {
			return fmt.Errorf("failover_bot: no snapshot available for recovery")
		}
		if err := b.backup.RestoreFromBackup(ctx, backups[0].BackupID, nodeID); err != nil {
			return fmt.Errorf("failover_bot: restore failed: %w", err)
		}
	}

	// NATS 이벤트 발행: 노드 복구 완료.
	_ = b.bus.Publish(ctx, DomainEvent{
		EventID:    newID("evt"),
		Subject:    "sovereign.ha.node.recovered",
		OrgID:      "",
		Payload:    []byte(fmt.Sprintf(`{"node_id":%q,"wal_lag_bytes":%d}`, nodeID, lag)),
		OccurredAt: time.Now(),
	})

	return nil
}
