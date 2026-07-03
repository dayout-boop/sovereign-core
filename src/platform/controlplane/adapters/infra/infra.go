package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────
// adapter_infra.go — 인프라 포트 어댑터 (초기 인메모리 구현)
//
// 설계 원칙:
//   - 초기: 인메모리 어댑터로 테스트 가능한 구조 확보.
//   - 프로덕션 교체: NATS JetStream 어댑터, Resilience4j 스타일 서킷 브레이커로 교체.
//   - 교체 시 이 파일만 수정하면 되며, 비즈니스 로직(app.go 등)은 무변경.
// ─────────────────────────────────────────────────────────────────────────

// ─── 인메모리 이벤트 버스 어댑터 ─────────────────────────────────────────

// InMemoryEventBus — 테스트 및 단일 노드 환경용 인메모리 이벤트 버스.
// 프로덕션에서는 NATSEventBus로 교체한다.
// 컴파일 타임 인터페이스 검증.
var _ EventBusPort = (*InMemoryEventBus)(nil)

type InMemoryEventBus struct {
	mu          sync.RWMutex
	subscribers map[string][]func(DomainEvent) error
	published   []DomainEvent // 테스트 검증용 이벤트 기록
}

func NewInMemoryEventBus() *InMemoryEventBus {
	return &InMemoryEventBus{
		subscribers: make(map[string][]func(DomainEvent) error),
	}
}

// Publish — 이벤트 발행. 동기적으로 구독자에게 전달 (인메모리 한정).
// NATS JetStream 어댑터에서는 비동기 발행 + at-least-once 보장.
func (b *InMemoryEventBus) Publish(ctx context.Context, event DomainEvent) error {
	if event.EventID == "" {
		return fmt.Errorf("event_id는 필수입니다 (멱등성 키)")
	}
	if event.Subject == "" {
		return fmt.Errorf("subject는 필수입니다")
	}

	b.mu.Lock()
	b.published = append(b.published, event)
	handlers := make([]func(DomainEvent) error, 0)
	for pattern, subs := range b.subscribers {
		if matchSubject(pattern, event.Subject) {
			handlers = append(handlers, subs...)
		}
	}
	b.mu.Unlock()

	// ctx 타임아웃 내 핸들러 실행
	for _, h := range handlers {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			if err := h(event); err != nil {
				// 인메모리에서는 오류 로깅만. NATS에서는 재전달.
				_ = err
			}
		}
	}
	return nil
}

// Subscribe — 주제 패턴 구독.
func (b *InMemoryEventBus) Subscribe(_ context.Context, subjectPattern string, handler func(DomainEvent) error) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subscribers[subjectPattern] = append(b.subscribers[subjectPattern], handler)
	return nil
}

// PublishedEvents — 테스트용: 발행된 이벤트 목록 반환.
func (b *InMemoryEventBus) PublishedEvents() []DomainEvent {
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make([]DomainEvent, len(b.published))
	copy(result, b.published)
	return result
}

// matchSubject — NATS 주제 패턴 매칭 (단순 구현: 완전 일치 또는 ">" 와일드카드).
func matchSubject(pattern, subject string) bool {
	if pattern == subject {
		return true
	}
	// "sovereign.>" 패턴: 접두사 매칭
	if len(pattern) > 2 && pattern[len(pattern)-2:] == ".>" {
		prefix := pattern[:len(pattern)-2]
		return len(subject) > len(prefix) && subject[:len(prefix)] == prefix
	}
	return false
}

// ─── 인메모리 서킷 브레이커 어댑터 ──────────────────────────────────────

// InMemoryCircuitBreaker — 단순 서킷 브레이커 구현.
// 프로덕션에서는 hystrix-go 또는 gobreaker 라이브러리로 교체한다.
// 컴파일 타임 인터페이스 검증.
var _ CircuitBreakerPort = (*InMemoryCircuitBreaker)(nil)

type circuitEntry struct {
	state        CircuitState
	failureCount int
	lastFailure  time.Time
	openUntil    time.Time
}

// InMemoryCircuitBreaker — 서비스별 서킷 브레이커 상태 관리.
type InMemoryCircuitBreaker struct {
	mu              sync.Mutex
	circuits        map[string]*circuitEntry
	failureThreshold int          // 이 횟수 이상 실패 시 Open
	openDuration    time.Duration // Open 상태 유지 시간
}

func NewInMemoryCircuitBreaker(failureThreshold int, openDuration time.Duration) *InMemoryCircuitBreaker {
	return &InMemoryCircuitBreaker{
		circuits:         make(map[string]*circuitEntry),
		failureThreshold: failureThreshold,
		openDuration:     openDuration,
	}
}

func (cb *InMemoryCircuitBreaker) getOrCreate(serviceName string) *circuitEntry {
	e, ok := cb.circuits[serviceName]
	if !ok {
		e = &circuitEntry{state: CircuitClosed}
		cb.circuits[serviceName] = e
	}
	return e
}

func (cb *InMemoryCircuitBreaker) State(_ context.Context, serviceName string) CircuitState {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	e := cb.getOrCreate(serviceName)
	// Open 상태에서 시간이 지나면 HalfOpen으로 전환
	if e.state == CircuitOpen && time.Now().After(e.openUntil) {
		e.state = CircuitHalfOpen
	}
	return e.state
}

func (cb *InMemoryCircuitBreaker) RecordSuccess(_ context.Context, serviceName string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	e := cb.getOrCreate(serviceName)
	e.failureCount = 0
	e.state = CircuitClosed
}

func (cb *InMemoryCircuitBreaker) RecordFailure(_ context.Context, serviceName string, _ error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	e := cb.getOrCreate(serviceName)
	e.failureCount++
	e.lastFailure = time.Now()
	if e.failureCount >= cb.failureThreshold {
		e.state = CircuitOpen
		e.openUntil = time.Now().Add(cb.openDuration)
	}
}

// ─── 인메모리 테넌트 프로비저닝 어댑터 ───────────────────────────────────

// InMemoryTenantProvisioner — 테스트용 테넌트 프로비저닝 어댑터.
// 프로덕션에서는 Neon API 어댑터로 교체한다.
// 컴파일 타임 인터페이스 검증.
var _ TenantProvisionPort = (*InMemoryTenantProvisioner)(nil)

type tenantRecord struct {
	orgID   string
	planID  string
	connStr string
	status  string
}

type InMemoryTenantProvisioner struct {
	mu      sync.RWMutex
	tenants map[string]*tenantRecord
}

func NewInMemoryTenantProvisioner() *InMemoryTenantProvisioner {
	return &InMemoryTenantProvisioner{
		tenants: make(map[string]*tenantRecord),
	}
}

func (p *InMemoryTenantProvisioner) ProvisionTenant(_ context.Context, orgID, planID string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.tenants[orgID]; exists {
		return p.tenants[orgID].connStr, nil // 멱등: 이미 존재하면 기존 반환
	}
	connStr := fmt.Sprintf("postgres://tenant_%s:secret@neon-internal/%s_db", orgID, orgID)
	p.tenants[orgID] = &tenantRecord{
		orgID:   orgID,
		planID:  planID,
		connStr: connStr,
		status:  "active",
	}
	return connStr, nil
}

func (p *InMemoryTenantProvisioner) DeprovisionTenant(_ context.Context, orgID string, immediate bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	rec, ok := p.tenants[orgID]
	if !ok {
		return nil // 멱등: 없으면 성공
	}
	if immediate {
		delete(p.tenants, orgID)
	} else {
		rec.status = "pending_deletion" // 30일 후 삭제 스케줄
	}
	return nil
}

func (p *InMemoryTenantProvisioner) GetTenantStatus(_ context.Context, orgID string) (string, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	rec, ok := p.tenants[orgID]
	if !ok {
		return "not_found", nil
	}
	return rec.status, nil
}
