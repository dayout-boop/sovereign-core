package main

// ─────────────────────────────────────────────────────────────────────────
// T8 — CustomerPort + MarginBillingAdapter 정합성.
//
// 명제:
//   T8-A 인터페이스 만족: MarginBillingAdapter 가 CustomerPort(L0)로 대입 가능.
//   T8-B 역마진 불가: 광범위 원가 스윗에서 sell > cost 항상 성립(마진 ≥ 1µc).
//   T8-C 멱등 고객: 같은 org 반복 CreateCustomer → 같은 billingID.
//   T8-D 결제→충전 정합: 동시 다발 SettleTopUp → 원장 적립 합 일치, 음수 0.
//   T8-E 마진율 0 경계: rate=0 이어도 동일가 판매 금지(최소 +1µc).
// ─────────────────────────────────────────────────────────────────────────

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

var _ CustomerPort = (*MarginBillingAdapter)(nil)

func TestStage2_T8A_InterfaceSatisfied(t *testing.T) {
	a := NewMarginBillingAdapter(NewLedger(), 0.10)
	var _ CustomerPort = a
	t.Log("[T8-A] MarginBillingAdapter 가 CustomerPort(L0) 계약 만족(실물 교체 가능)")
}

func TestStage2_T8B_NoNegativeMargin(t *testing.T) {
	a := NewMarginBillingAdapter(NewLedger(), 0.10)
	// 원가 1µc ~ 큰 값까지 광범위 스윕 + 마진율 여러 개.
	rates := []float64{0.001, 0.01, 0.10, 0.30, 1.0}
	costs := []int64{1, 2, 3, 7, 99, 100, 333, 1000, 999_999, 1_000_000, 7_777_777}
	for _, r := range rates {
		a.marginRate = r
		for _, c := range costs {
			a.SetCost("sku", c)
			sell, margin, err := a.Quote("sku")
			if err != nil {
				t.Fatalf("quote err: %v", err)
			}
			if sell <= c {
				t.Fatalf("[T8-B 결함] 역마진: rate=%.3f cost=%d sell=%d", r, c, sell)
			}
			if margin < 1 {
				t.Fatalf("[T8-B 결함] 마진 <1µc: rate=%.3f cost=%d margin=%d", r, c, margin)
			}
		}
	}
	t.Log("[T8-B] 원가×마진율 전 조합에서 sell>cost, margin≥1µc (역마진 0건)")
}

func TestStage2_T8C_IdempotentCustomer(t *testing.T) {
	a := NewMarginBillingAdapter(NewLedger(), 0.10)
	ctx := context.Background()
	id1, _ := a.CreateCustomer(ctx, "orgA")
	id2, _ := a.CreateCustomer(ctx, "orgA")
	if id1 != id2 {
		t.Fatalf("[T8-C 결함] 같은 org 인데 billingID 상이: %s vs %s", id1, id2)
	}
	t.Logf("[T8-C] 멱등 고객 생성 확인: %s", id1)
}

func TestStage2_T8D_SettleToLedgerConsistency(t *testing.T) {
	l := NewLedger()
	a := NewMarginBillingAdapter(l, 0.10)

	const N = 200
	const each int64 = 250_000 // 0.25 크레딧
	var okCount int64
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if a.SettleTopUp("orgA", each) == nil {
				atomic.AddInt64(&okCount, 1)
			}
		}()
	}
	wg.Wait()

	want := int64(okCount) * each
	got := l.BalanceMicro("orgA")
	consistent, neg := l.auditConsistent()
	t.Logf("[T8-D] 정산 %d건 → 원장 %dµc (기대 %dµc), 감사=%v 음수=%d", okCount, got, want, consistent, neg)
	if got != want {
		t.Fatalf("[T8-D 결함] 결제→충전 불일치: got %dµc want %dµc", got, want)
	}
	if !consistent || neg != 0 {
		t.Fatalf("[T8-D 결함] 원장 감사 실패 consistent=%v neg=%d", consistent, neg)
	}
}

func TestStage2_T8E_ZeroRateStillMargin(t *testing.T) {
	a := NewMarginBillingAdapter(NewLedger(), 0.0) // 마진율 0
	for _, c := range []int64{1, 5, 100, 1_000_000} {
		a.SetCost("sku", c)
		sell, margin, _ := a.Quote("sku")
		if sell <= c || margin < 1 {
			t.Fatalf("[T8-E 결함] rate=0 에서도 동일가 판매 금지여야: cost=%d sell=%d", c, sell)
		}
	}
	t.Log("[T8-E] marginRate=0 이어도 최소 +1µc 마진 강제(동일가 판매 금지)")
}

// 참고 출력: 대표 원가에 대한 견적 예시(사람이 눈으로 확인).
func TestStage2_T8F_QuoteExample(t *testing.T) {
	a := NewMarginBillingAdapter(NewLedger(), 0.10)
	a.SetCost("llm.infer.1k", 1_500) // 원가 0.0015 크레딧
	sell, margin, _ := a.Quote("llm.infer.1k")
	t.Log(fmt.Sprintf("[T8-F 예시] llm.infer.1k 원가=1500µc → 판매=%dµc 마진=%dµc(10%%)", sell, margin))
}
