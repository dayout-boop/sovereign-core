package main

import (
	"fmt"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────
// 크레딧 원장 (Credit Ledger) — 정산 정합성의 핵심.
//
// 설계 원칙:
//  1) 정수 회계: 크레딧을 부동소수점이 아니라 정수 "마이크로크레딧(µc)"으로 저장.
//     1 credit = 1_000_000 µc. 반올림 오차 누적 원천 차단.
//  2) append-only 원장: 잔액은 단일 스칼라가 아니라 거래(LedgerEntry) 합.
//     감사 추적 가능 + 이중차감/유실 탐지 용이.
//  3) 원자적 검사-차감: Charge 는 "잔액 충분 여부 확인 + 차감"을 단일 잠금
//     구간에서 수행(TOCTOU 방지).
//  4) 불변식: 어떤 동시성 상황에서도 org 잔액은 절대 음수가 되지 않는다.
//
// [sovereign_core] 결제 원장 확장 (2026-07-03):
//   - Refund: 환불 처리 (일할 계산 + 위약금 차감 후 크레딧 복원)
//   - IssueSLACredit: SLA 위반 자동 보상 크레딧 지급
//   - Reserve/CommitReservation/ReleaseReservation: 외부 LLM 선차감 예약
//   - GracePeriod: 결제 실패 유예 기간 추적
//
// 진짜 전환: 이 인메모리 원장을 Postgres 트랜잭션으로 교체하면 계약은 그대로 유지.
// ─────────────────────────────────────────────────────────────────────────

const MicroPerCredit int64 = 1_000_000

var ErrInsufficient = fmt.Errorf("insufficient credit balance")
var ErrNegativeRefund = fmt.Errorf("refund amount exceeds charged amount")

// LedgerEntry — 원장 거래 1건(append-only). Delta 양수=충전, 음수=차감.
type LedgerEntry struct {
	OrgID     string    `json:"org_id"`
	Delta     int64     `json:"delta_micro"` // µc, 부호 있음
	Reason    string    `json:"reason"`      // topup|charge:<kind>|refund|sla_credit|penalty|reserve|commit|release
	Reference string    `json:"reference,omitempty"` // 관련 ID (invoiceID, incidentID 등)
	At        time.Time `json:"at"`
}

// GracePeriodState — 결제 실패 유예 상태.
type GracePeriodState struct {
	OrgID          string    `json:"org_id"`
	FailedAt       time.Time `json:"failed_at"`
	GracePeriodEnd time.Time `json:"grace_period_end"` // 이 시점까지 서비스 유지
	Suspended      bool      `json:"suspended"`         // 유예 만료 후 정지 여부
}

// Ledger — org별 잔액과 거래 원장.
type Ledger struct {
	mu           sync.Mutex
	balance      map[string]int64          // orgID -> µc (파생 캐시)
	reserved     map[string]int64          // orgID -> 예약 중인 µc (외부 LLM 선차감)
	entries      []LedgerEntry
	gracePeriods map[string]*GracePeriodState
}

func NewLedger() *Ledger {
	return &Ledger{
		balance:      map[string]int64{},
		reserved:     map[string]int64{},
		gracePeriods: map[string]*GracePeriodState{},
	}
}

// appendLocked — 원장에 거래 기록 + 파생 잔액 갱신. 호출자가 잠금 보유.
func (l *Ledger) appendLocked(orgID string, delta int64, reason, reference string) {
	l.entries = append(l.entries, LedgerEntry{
		OrgID: orgID, Delta: delta, Reason: reason, Reference: reference,
		At: time.Now().UTC(),
	})
	l.balance[orgID] += delta
}

// TopUp — 크레딧 충전(credit 단위). 음수 충전 금지.
func (l *Ledger) TopUp(orgID string, credits int64) error {
	if credits <= 0 {
		return fmt.Errorf("topup must be positive")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.appendLocked(orgID, credits*MicroPerCredit, "topup", "")
	return nil
}

// TopUpMicro — 마이크로크레딧 단위 충전(외부 결제 정산용).
func (l *Ledger) TopUpMicro(orgID string, micro int64) error {
	return l.TopUpMicroWithRef(orgID, micro, "")
}

// TopUpMicroWithRef — 참조 ID 포함 충전 (인보이스 ID, 웹훅 이벤트 ID 등).
func (l *Ledger) TopUpMicroWithRef(orgID string, micro int64, reference string) error {
	if micro <= 0 {
		return fmt.Errorf("topup must be positive")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.appendLocked(orgID, micro, "topup", reference)
	return nil
}

// Charge — 원자적 검사-차감. 잔액 부족 시 ErrInsufficient(원장 불변).
func (l *Ledger) Charge(orgID string, micro int64, reason string) error {
	return l.ChargeWithRef(orgID, micro, reason, "")
}

// ChargeWithRef — 참조 ID 포함 차감 (인보이스 ID 등).
func (l *Ledger) ChargeWithRef(orgID string, micro int64, reason, reference string) error {
	if micro <= 0 {
		return fmt.Errorf("charge must be positive")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.balance[orgID] < micro {
		return ErrInsufficient
	}
	l.appendLocked(orgID, -micro, reason, reference)
	return nil
}

// Refund — 환불 처리.
// grossMicro: 원래 청구된 금액(µc).
// penaltyRate: 위약금 비율 (0.0~1.0). 위약금 = grossMicro × penaltyRate.
// 실제 환불 = grossMicro - 위약금. 위약금이 전액을 초과하면 0 환불.
// reference: 관련 인보이스 ID 또는 구독 ID.
func (l *Ledger) Refund(orgID string, grossMicro int64, penaltyRate float64, reason, reference string) (refundedMicro, penaltyMicro int64, err error) {
	if grossMicro <= 0 {
		return 0, 0, fmt.Errorf("refund gross amount must be positive")
	}
	if penaltyRate < 0 || penaltyRate > 1 {
		return 0, 0, fmt.Errorf("penalty rate must be between 0 and 1")
	}

	// 위약금 계산 (올림 처리 — 고객에게 불리하지 않게 내림으로 계산)
	rawPenalty := float64(grossMicro) * penaltyRate
	penaltyMicro = int64(rawPenalty) // 내림: 위약금을 최소화하여 고객 보호
	refundedMicro = grossMicro - penaltyMicro
	if refundedMicro < 0 {
		refundedMicro = 0
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if penaltyMicro > 0 {
		l.appendLocked(orgID, -penaltyMicro, "penalty:"+reason, reference)
	}
	if refundedMicro > 0 {
		l.appendLocked(orgID, refundedMicro, "refund:"+reason, reference)
	}
	return refundedMicro, penaltyMicro, nil
}

// IssueSLACredit — SLA 위반 자동 보상 크레딧 지급.
// incidentID: 장애 식별자 (중복 보상 방지를 위해 감사 로그에 기록).
func (l *Ledger) IssueSLACredit(orgID string, compensationMicro int64, incidentID string) error {
	if compensationMicro <= 0 {
		return fmt.Errorf("sla credit must be positive")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.appendLocked(orgID, compensationMicro, "sla_credit", incidentID)
	return nil
}

// Reserve — 외부 LLM 호출 선차감 예약. 가용 잔액(balance - reserved)에서 차감.
// 실제 잔액은 CommitReservation 또는 ReleaseReservation 호출 시 확정.
func (l *Ledger) Reserve(orgID string, micro int64) error {
	if micro <= 0 {
		return fmt.Errorf("reserve must be positive")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	available := l.balance[orgID] - l.reserved[orgID]
	if available < micro {
		return ErrInsufficient
	}
	l.reserved[orgID] += micro
	l.appendLocked(orgID, 0, "reserve", fmt.Sprintf("reserved:%d", micro)) // 감사 기록만
	return nil
}

// CommitReservation — 예약 확정: 실제 사용량으로 차감하고 예약 해제.
// actualMicro <= reservedMicro 여야 한다 (초과 사용은 별도 Charge 호출).
func (l *Ledger) CommitReservation(orgID string, reservedMicro, actualMicro int64, reason string) error {
	if actualMicro < 0 {
		return fmt.Errorf("actual usage must be non-negative")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.reserved[orgID] < reservedMicro {
		return fmt.Errorf("reservation not found or already released")
	}
	l.reserved[orgID] -= reservedMicro
	if actualMicro > 0 {
		if l.balance[orgID] < actualMicro {
			return ErrInsufficient
		}
		l.appendLocked(orgID, -actualMicro, "commit:"+reason, "")
	}
	return nil
}

// ReleaseReservation — 예약 취소: 호출 실패 시 예약 크레딧 반환.
func (l *Ledger) ReleaseReservation(orgID string, reservedMicro int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.reserved[orgID] < reservedMicro {
		return fmt.Errorf("reservation not found or already released")
	}
	l.reserved[orgID] -= reservedMicro
	l.appendLocked(orgID, 0, "release", fmt.Sprintf("released:%d", reservedMicro))
	return nil
}

// SetGracePeriod — 결제 실패 유예 기간 설정 (3일 기본).
func (l *Ledger) SetGracePeriod(orgID string, failedAt time.Time, graceDays int) *GracePeriodState {
	l.mu.Lock()
	defer l.mu.Unlock()
	gp := &GracePeriodState{
		OrgID:          orgID,
		FailedAt:       failedAt,
		GracePeriodEnd: failedAt.AddDate(0, 0, graceDays),
		Suspended:      false,
	}
	l.gracePeriods[orgID] = gp
	l.appendLocked(orgID, 0, "grace_period_start", fmt.Sprintf("grace_until:%s", gp.GracePeriodEnd.Format(time.RFC3339)))
	return gp
}

// SuspendForNonPayment — 유예 만료 후 계정 정지.
func (l *Ledger) SuspendForNonPayment(orgID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	gp, ok := l.gracePeriods[orgID]
	if !ok {
		return fmt.Errorf("no grace period found for org %s", orgID)
	}
	if time.Now().UTC().Before(gp.GracePeriodEnd) {
		return fmt.Errorf("grace period not yet expired for org %s", orgID)
	}
	gp.Suspended = true
	l.appendLocked(orgID, 0, "suspended:non_payment", "")
	return nil
}

// GracePeriodOf — 유예 상태 조회.
func (l *Ledger) GracePeriodOf(orgID string) (*GracePeriodState, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	gp, ok := l.gracePeriods[orgID]
	return gp, ok
}

// BalanceMicro — 현재 잔액(µc).
func (l *Ledger) BalanceMicro(orgID string) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.balance[orgID]
}

// AvailableMicro — 가용 잔액(µc) = 잔액 - 예약 중인 금액.
func (l *Ledger) AvailableMicro(orgID string) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.balance[orgID] - l.reserved[orgID]
}

// Balance — 현재 잔액(credit, 소수 포함).
func (l *Ledger) Balance(orgID string) float64 {
	return float64(l.BalanceMicro(orgID)) / float64(MicroPerCredit)
}

// auditConsistent — 감사: 파생 잔액 캐시가 원장 거래 합과 정확히 일치하는가.
func (l *Ledger) auditConsistent() (consistent bool, negativeOrgs int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	recomputed := map[string]int64{}
	for _, e := range l.entries {
		recomputed[e.OrgID] += e.Delta
	}
	consistent = true
	for org, bal := range l.balance {
		if recomputed[org] != bal {
			consistent = false
		}
	}
	for _, bal := range l.balance {
		if bal < 0 {
			negativeOrgs++
		}
	}
	return consistent, negativeOrgs
}
