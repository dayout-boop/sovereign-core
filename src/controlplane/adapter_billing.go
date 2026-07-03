package main

import (
	"context"
	"fmt"
	"sync"
)

// ─────────────────────────────────────────────────────────────────────────
// MarginBillingAdapter — CustomerPort + 마진 과금 어댑터 (초기 부트스트랩 트랙).
//
// 구현 포트: CustomerPort (L0만 구현)
//   - 구독/인보이스/환불은 MultiPGPaymentAdapter가 담당.
//   - 이 어댑터는 "원가 + 마진 → 판매가 계산" 및 "크레딧 충전 정산"에 집중.
//
// 설계 원칙:
//   1) 역마진 불가: 판매가는 항상 원가보다 크다(sell > cost).
//   2) 정수 회계: 원가/판매가 모두 마이크로크레딧(µc) 정수. 반올림은 항상 "올림".
//   3) 결제→충전 정합: 정산액이 그대로 크레딧 원장(Ledger)에 적립.
//
// 진짜 전환: CreateCustomer를 실제 결제사(Stripe/Toss) SDK 호출로 교체.
//            계약(CustomerPort)과 마진 로직은 그대로 유지.
// ─────────────────────────────────────────────────────────────────────────

// UpstreamCost — 상류 공급자 원가(µc). 예: LLM 추론 1K 토큰당 원가.
type UpstreamCost struct {
	SKU       string // 예: "llm.infer.1k", "gpu.sec", "storage.gb.month"
	CostMicro int64  // µc, 원가(> 0)
}

type MarginBillingAdapter struct {
	mu         sync.Mutex
	ledger     *Ledger
	marginRate float64           // 예: 0.10 = 10% 마진
	costTable  map[string]int64  // SKU -> 원가(µc)
	customers  map[string]string // orgID -> billingID
}

func NewMarginBillingAdapter(ledger *Ledger, marginRate float64) *MarginBillingAdapter {
	return &MarginBillingAdapter{
		ledger:     ledger,
		marginRate: marginRate,
		costTable:  map[string]int64{},
		customers:  map[string]string{},
	}
}

// SetCost — 원가표에 SKU 등록/갱신.
func (a *MarginBillingAdapter) SetCost(sku string, costMicro int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.costTable[sku] = costMicro
}

// ceilMul — costMicro × (1 + rate) 를 정수 올림(µc). 역마진 방지의 핵심.
// 올림이므로 결과는 항상 원가 이상이며, rate>0 이면 항상 원가 초과.
// rate 가 0 또는 반올림으로 0 마진이 나와도 최소 +1µc 를 보장한다.
func ceilMul(costMicro int64, rate float64) int64 {
	if costMicro <= 0 {
		return 0
	}
	raw := float64(costMicro) * (1.0 + rate)
	sell := int64(raw)
	if float64(sell) < raw {
		sell++
	}
	if sell <= costMicro {
		sell = costMicro + 1
	}
	return sell
}

// Quote — SKU 의 판매가(µc)와 마진(µc)을 계산. 등록 안된 SKU 는 오류.
func (a *MarginBillingAdapter) Quote(sku string) (sellMicro, marginMicro int64, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cost, ok := a.costTable[sku]
	if !ok {
		return 0, 0, fmt.Errorf("unknown sku: %s", sku)
	}
	sell := ceilMul(cost, a.marginRate)
	return sell, sell - cost, nil
}

// CreateCustomer — CustomerPort 구현. 멱등(같은 org 는 같은 billingID).
// 진짜: Stripe/Toss Customer API 호출.
func (a *MarginBillingAdapter) CreateCustomer(_ context.Context, orgID string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if id, ok := a.customers[orgID]; ok {
		return id, nil // 멱등
	}
	id := newID("cus")
	a.customers[orgID] = id
	return id, nil
}

// SettleTopUp — 결제 성공분(µc)을 크레딧 원장에 적립(결제→충전 정합).
// paidMicro 는 실제 수납액(판매가 기준). 원장 적립과 1:1.
func (a *MarginBillingAdapter) SettleTopUp(orgID string, paidMicro int64) error {
	if paidMicro <= 0 {
		return fmt.Errorf("settle amount must be positive")
	}
	return a.ledger.TopUpMicro(orgID, paidMicro)
}

// 컴파일 타임 인터페이스 준수 검증.
var _ CustomerPort = (*MarginBillingAdapter)(nil)
