package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────
// 웜풀 (D7 핫패스) — 즉시성 갭의 해법.
// 컴퓨트를 미리 부팅해두고(BootInstance), 요청 시 꺼내 붙인다(AttachBranch).
//   - 히트: 데워둔 인스턴스 → boot_ms = 0
//   - 미스: 풀 비어 콜드부팅 → boot_ms = 실측(진짜는 PVM/Firecracker)
// 정책은 인터페이스(다중 선택). 기본 = 부하 비례.
// ─────────────────────────────────────────────────────────────────────────

// WarmPoolPolicy — 목표 풀 크기를 정한다. 갈아끼우는 전략.
type WarmPoolPolicy interface {
	Target(recentAcquires int) int // 최근 획득 수 → 데워둘 개수
	Name() string
}

// (가) 고정 크기 — 상시 N개 대기. 단순·예측가능. 버스트엔 미스/평시엔 낭비.
type FixedPolicy struct{ N int }

func (p FixedPolicy) Target(_ int) int { return p.N }
func (p FixedPolicy) Name() string     { return fmt.Sprintf("fixed(N=%d)", p.N) }

// (나) 부하 비례 — 최근 생성률 × 버퍼, [Min,Max] 클램프. 기본값(추천).
// 에이전트 버스트성 워크로드에 자동 적응. 실제 Buffer/Min/Max 는 PoC로 확정.
type LoadProportionalPolicy struct {
	Buffer   float64
	Min, Max int
}

func (p LoadProportionalPolicy) Target(recent int) int {
	t := int(float64(recent) * p.Buffer)
	if t < p.Min {
		t = p.Min
	}
	if t > p.Max {
		t = p.Max
	}
	return t
}
func (p LoadProportionalPolicy) Name() string {
	return fmt.Sprintf("load-proportional(buf=%.1f,min=%d,max=%d)", p.Buffer, p.Min, p.Max)
}

type WarmInstance struct {
	ID   string
	Host string
}

type WarmPool struct {
	mu      sync.Mutex
	ready   []WarmInstance
	storage StoragePort
	region  string
	policy  WarmPoolPolicy
	hits    int
	misses  int
	recent  []time.Time // 획득 타임스탬프(수요 신호)
}

func NewWarmPool(storage StoragePort, region string, policy WarmPoolPolicy) *WarmPool {
	p := &WarmPool{storage: storage, region: region, policy: policy}
	p.mu.Lock()
	p.replenishLocked(context.Background())
	p.mu.Unlock()
	return p
}

// 최근 1분 내 획득 수(수요 신호). 호출자가 잠금 보유.
func (p *WarmPool) recentAcquiresLocked() int {
	cutoff := time.Now().Add(-time.Minute)
	n := 0
	for _, t := range p.recent {
		if t.After(cutoff) {
			n++
		}
	}
	return n
}

// 정책 목표까지 데워둔다. 호출자가 잠금 보유.
func (p *WarmPool) replenishLocked(ctx context.Context) {
	target := p.policy.Target(p.recentAcquiresLocked())
	for len(p.ready) < target {
		id, host, _, err := p.storage.BootInstance(ctx, p.region)
		if err != nil {
			break
		}
		p.ready = append(p.ready, WarmInstance{ID: id, Host: host})
	}
}

// Acquire — 컴퓨트 하나 확보. 히트(boot 0) 또는 미스(콜드부팅 실측).
func (p *WarmPool) Acquire(ctx context.Context) (inst WarmInstance, hit bool, bootMS int64, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.recent = append(p.recent, time.Now())
	if len(p.ready) > 0 {
		inst = p.ready[0]
		p.ready = p.ready[1:]
		p.hits++
		p.replenishLocked(ctx) // 백필
		return inst, true, 0, nil
	}
	// 미스: 콜드부팅
	id, host, bootMS, err := p.storage.BootInstance(ctx, p.region)
	if err != nil {
		return WarmInstance{}, false, bootMS, err
	}
	p.misses++
	p.replenishLocked(ctx)
	return WarmInstance{ID: id, Host: host}, false, bootMS, nil
}

func (p *WarmPool) Stats() (ready, hits, misses int, policy string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.ready), p.hits, p.misses, p.policy.Name()
}

// Drain — 데워둔 것 비움(테스트: 다음 Acquire 를 미스로 강제).
func (p *WarmPool) Drain() {
	p.mu.Lock()
	p.ready = nil
	p.mu.Unlock()
}
