package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────
// adapter_operator.go — OperatorPort 구현체
//
// 설계 원칙:
//   - 모든 금액 변경 액션은 4-eyes(2인 승인) 필수.
//   - 모든 액션은 NATS audit 스트림에 불변 기록.
//   - 운영자는 외부 PG 대시보드에 직접 접근하지 않는다.
//   - 동일 운영자가 요청+승인 동시 불가 (자기 승인 금지).
// ─────────────────────────────────────────────────────────────────────────

// operatorRequest — 운영자 처리 요청 내부 레코드.
type operatorRequest struct {
	RequestID   string
	OperatorID  string
	Action      OperatorAction
	TargetID    string
	Reason      string
	AmountMicro int64
	Status      string // "pending_approval" | "approved" | "rejected" | "executed"
	ApproverID  string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// OperatorAdapter — OperatorPort 인메모리 구현체.
type OperatorAdapter struct {
	mu       sync.RWMutex
	requests map[string]*operatorRequest // requestID → request
	auditLog []AuditEvent
	bus      EventBusPort
}

// 컴파일 타임 인터페이스 계약 검증.
var _ OperatorPort = (*OperatorAdapter)(nil)

// NewOperatorAdapter — OperatorAdapter 생성.
func NewOperatorAdapter(bus EventBusPort) *OperatorAdapter {
	return &OperatorAdapter{
		requests: make(map[string]*operatorRequest),
		bus:      bus,
	}
}

// RequestAction — 1차 운영자 처리 요청.
// 금액 변경 액션(환불, 크레딧 조정)은 승인 대기 상태 반환.
// 비금액 액션(DLQ 재처리 등)은 즉시 실행.
func (a *OperatorAdapter) RequestAction(
	ctx context.Context,
	operatorID string,
	action OperatorAction,
	targetID, reason string,
	amountMicro int64,
) (string, bool, error) {
	if operatorID == "" {
		return "", false, fmt.Errorf("operator: operatorID required")
	}
	if targetID == "" {
		return "", false, fmt.Errorf("operator: targetID required")
	}
	if reason == "" {
		return "", false, fmt.Errorf("operator: reason required")
	}

	// 금액 변경 액션은 4-eyes 승인 필수.
	requiresApproval := amountMicro != 0 ||
		action == OpActionRefund ||
		action == OpActionCreditAdjust

	requestID := newID("opreq")
	status := "pending_approval"
	if !requiresApproval {
		status = "executed"
	}

	req := &operatorRequest{
		RequestID:   requestID,
		OperatorID:  operatorID,
		Action:      action,
		TargetID:    targetID,
		Reason:      reason,
		AmountMicro: amountMicro,
		Status:      status,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	a.mu.Lock()
	a.requests[requestID] = req
	a.mu.Unlock()

	// 감사 로그 기록.
	a.appendAudit(AuditEvent{
		EventID:    newID("audit"),
		OperatorID: operatorID,
		Action:     action,
		TargetID:   targetID,
		Reason:     reason,
		OccurredAt: time.Now(),
	})

	// NATS 이벤트 발행.
	_ = a.bus.Publish(ctx, DomainEvent{
		EventID:    newID("evt"),
		Subject:    "sovereign.operator.action.requested",
		OrgID:      targetID,
		Payload:    []byte(fmt.Sprintf(`{"request_id":%q,"action":%q,"requires_approval":%v}`, requestID, action, requiresApproval)),
		OccurredAt: time.Now(),
	})

	return requestID, requiresApproval, nil
}

// ApproveAction — 2차 시니어 운영자 승인 (4-eyes).
func (a *OperatorAdapter) ApproveAction(ctx context.Context, approverID, requestID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	req, ok := a.requests[requestID]
	if !ok {
		return fmt.Errorf("operator: request not found: %s", requestID)
	}
	if req.Status != "pending_approval" {
		return fmt.Errorf("operator: request not in pending_approval state: %s", req.Status)
	}
	// 자기 승인 금지 (4-eyes 원칙).
	if req.OperatorID == approverID {
		return fmt.Errorf("operator: self-approval forbidden (4-eyes principle)")
	}

	req.Status = "approved"
	req.ApproverID = approverID
	req.UpdatedAt = time.Now()

	// 감사 로그 기록.
	a.appendAuditLocked(AuditEvent{
		EventID:    newID("audit"),
		OperatorID: approverID,
		Action:     req.Action,
		TargetID:   req.TargetID,
		Reason:     fmt.Sprintf("approved request %s", requestID),
		ApproverID: approverID,
		OccurredAt: time.Now(),
	})

	// NATS 이벤트 발행.
	_ = a.bus.Publish(ctx, DomainEvent{
		EventID:    newID("evt"),
		Subject:    "sovereign.operator.action.approved",
		OrgID:      req.TargetID,
		Payload:    []byte(fmt.Sprintf(`{"request_id":%q,"approver_id":%q}`, requestID, approverID)),
		OccurredAt: time.Now(),
	})

	return nil
}

// RejectAction — 2차 승인자 거부.
func (a *OperatorAdapter) RejectAction(ctx context.Context, approverID, requestID, reason string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	req, ok := a.requests[requestID]
	if !ok {
		return fmt.Errorf("operator: request not found: %s", requestID)
	}
	if req.Status != "pending_approval" {
		return fmt.Errorf("operator: request not in pending_approval state: %s", req.Status)
	}
	if req.OperatorID == approverID {
		return fmt.Errorf("operator: self-rejection forbidden")
	}

	req.Status = "rejected"
	req.ApproverID = approverID
	req.UpdatedAt = time.Now()

	a.appendAuditLocked(AuditEvent{
		EventID:    newID("audit"),
		OperatorID: approverID,
		Action:     req.Action,
		TargetID:   req.TargetID,
		Reason:     fmt.Sprintf("rejected: %s", reason),
		ApproverID: approverID,
		OccurredAt: time.Now(),
	})

	return nil
}

// GetAuditLog — 감사 로그 조회 (targetID 기준, 시간 범위 필터).
func (a *OperatorAdapter) GetAuditLog(ctx context.Context, targetID string, from, to time.Time) ([]AuditEvent, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	var result []AuditEvent
	for _, evt := range a.auditLog {
		if evt.TargetID != targetID {
			continue
		}
		if !from.IsZero() && evt.OccurredAt.Before(from) {
			continue
		}
		if !to.IsZero() && evt.OccurredAt.After(to) {
			continue
		}
		result = append(result, evt)
	}
	return result, nil
}

// appendAudit — 감사 로그 추가 (락 획득).
func (a *OperatorAdapter) appendAudit(evt AuditEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.auditLog = append(a.auditLog, evt)
}

// appendAuditLocked — 감사 로그 추가 (락 보유 상태에서 호출).
func (a *OperatorAdapter) appendAuditLocked(evt AuditEvent) {
	a.auditLog = append(a.auditLog, evt)
}
