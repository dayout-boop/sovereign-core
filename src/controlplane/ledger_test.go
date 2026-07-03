package main

// ─────────────────────────────────────────────────────────────────────────
// T7 — 크레딧 원장 정합성(정산의 남은 절반). "음수 잔액 0건" SLO 증명.
//
// 명제:
//   T7-A 원자적 검사-차감: 잔액 10, 동시 20건 각 1크레딧 차감 → 정확히 10 성공/10 거부,
//        최종 잔액 0, 음수 없음.
//   T7-B 음수 불가(정밀): 잔액 1µc, 동시 다발 1µc 차감 → 1건만 성공, 잔액 0, 절대 음수 없음.
//   T7-C 감사 일치: 대량 랜덤 topup/charge 후 파생 잔액 == 원장 거래 합(이중차감/유실 0).
//   T7-D 정수 회계: 0.1 크레딧 10회 차감 = 정확히 1 크레딧(부동소수 오차 0).
// ─────────────────────────────────────────────────────────────────────────

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestStage2_T7A_AtomicChargeNoNegative(t *testing.T) {
	l := NewLedger()
	_ = l.TopUp("orgA", 10) // 10 크레딧

	const concurrent = 20
	var ok, rejected int64
	var wg sync.WaitGroup
	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := l.Charge("orgA", 1*MicroPerCredit, "charge:cu")
			if err == nil {
				atomic.AddInt64(&ok, 1)
			} else {
				atomic.AddInt64(&rejected, 1)
			}
		}()
	}
	wg.Wait()

	bal := l.BalanceMicro("orgA")
	t.Logf("[T7-A] 성공=%d 거부=%d 최종잔액=%dµc", ok, rejected, bal)
	if ok != 10 || rejected != 10 {
		t.Fatalf("[T7-A 결함] 잔액10에 20건 → 성공 10/거부 10 이어야: got ok=%d rej=%d", ok, rejected)
	}
	if bal != 0 {
		t.Fatalf("[T7-A 결함] 최종 잔액 0 이어야: got %dµc", bal)
	}
	if bal < 0 {
		t.Fatalf("[T7-A 치명] 잔액 음수 발생: %dµc", bal)
	}
}

func TestStage2_T7B_NoNegativePrecise(t *testing.T) {
	l := NewLedger()
	_ = l.TopUpMicro("orgB", 1) // 1µc 만

	const concurrent = 50
	var ok int64
	var wg sync.WaitGroup
	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.Charge("orgB", 1, "charge:micro") == nil {
				atomic.AddInt64(&ok, 1)
			}
		}()
	}
	wg.Wait()

	bal := l.BalanceMicro("orgB")
	t.Logf("[T7-B] 1µc 에 50건 동시 → 성공=%d 잔액=%dµc", ok, bal)
	if ok != 1 {
		t.Fatalf("[T7-B 결함] 정확히 1건만 성공해야: got %d", ok)
	}
	if bal != 0 {
		t.Fatalf("[T7-B 결함] 잔액 0 이어야: got %dµc", bal)
	}
}

func TestStage2_T7C_AuditConsistency(t *testing.T) {
	l := NewLedger()
	orgs := []string{"o1", "o2", "o3", "o4", "o5"}
	for _, o := range orgs {
		_ = l.TopUp(o, 100)
	}

	var wg sync.WaitGroup
	// 각 org 에 대해 동시 topup/charge 를 뒤섞어 발사.
	for _, o := range orgs {
		for i := 0; i < 200; i++ {
			wg.Add(1)
			go func(org string, n int) {
				defer wg.Done()
				if n%2 == 0 {
					_ = l.Charge(org, 1*MicroPerCredit, "charge:mix")
				} else {
					_ = l.TopUpMicro(org, 500_000) // 0.5 크레딧
				}
			}(o, i)
		}
	}
	wg.Wait()

	consistent, neg := l.auditConsistent()
	t.Logf("[T7-C] 감사 일치=%v 음수org수=%d", consistent, neg)
	if !consistent {
		t.Fatalf("[T7-C 결함] 파생 잔액이 원장 거래 합과 불일치(이중차감/유실)")
	}
	if neg != 0 {
		t.Fatalf("[T7-C 치명] 음수 잔액 org 발생: %d", neg)
	}
}

func TestStage2_T7D_IntegerAccounting(t *testing.T) {
	l := NewLedger()
	_ = l.TopUp("orgD", 1) // 1 크레딧 = 1,000,000 µc

	// 0.1 크레딧(100,000 µc) 10회 차감 = 정확히 1 크레딧.
	for i := 0; i < 10; i++ {
		if err := l.Charge("orgD", 100_000, "charge:tenth"); err != nil {
			t.Fatalf("[T7-D] %d회차 차감 실패: %v", i, err)
		}
	}
	bal := l.BalanceMicro("orgD")
	if bal != 0 {
		t.Fatalf("[T7-D 결함] 0.1×10 = 1.0 정확히 소진되어야: 잔액 %dµc(부동소수 오차?)", bal)
	}
	// 11번째는 잔액 부족으로 거부되어야.
	if err := l.Charge("orgD", 1, "charge:over"); err != ErrInsufficient {
		t.Fatalf("[T7-D 결함] 잔액 0 에서 추가 차감은 거부여야: got %v", err)
	}
	t.Log("[T7-D] 정수 회계 정확: 0.1×10=1.0, 오차 0, 초과차감 거부")
}
