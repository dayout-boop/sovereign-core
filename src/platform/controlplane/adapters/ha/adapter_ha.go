package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────
// adapter_ha.go — HA 포트 인메모리 구현체
//
// 포함:
//   - HeartbeatAdapter: Phi-accrual 장애 감지
//   - FailoverAdapter: Quorum 기반 자동 페일오버 + STONITH Fencing
//   - BackupAdapter: 스냅샷/WAL 백업 스케줄링 및 복원
// ─────────────────────────────────────────────────────────────────────────

// validNodeRoles — 허용된 NodeRole 집합.
var validNodeRoles = map[NodeRole]bool{
	NodeRoleActive:  true,
	NodeRoleStandby: true,
	NodeRoleWitness: true,
	NodeRoleUnknown: true,
}

// ─── HeartbeatAdapter ────────────────────────────────────────────────────

// phiState — Phi-accrual 장애 감지 상태.
type phiState struct {
	lastSeen  time.Time
	intervals []time.Duration // 최근 하트비트 간격 이력
}

// phi — Phi-accrual 장애 점수 계산 (단순 근사).
// 실제 구현에서는 정규분포 기반 누적분포함수(CDF)를 사용.
func (s *phiState) phi() float64 {
	if s.lastSeen.IsZero() {
		return 0
	}
	elapsed := time.Since(s.lastSeen)
	if len(s.intervals) == 0 {
		return 0
	}
	// 평균 간격 계산.
	var sum time.Duration
	for _, d := range s.intervals {
		sum += d
	}
	mean := sum / time.Duration(len(s.intervals))
	if mean == 0 {
		return 0
	}
	// Phi = elapsed / mean (단순 근사; 실제: -log(1 - CDF(elapsed))).
	return float64(elapsed) / float64(mean)
}

// HeartbeatAdapter — HeartbeatPort 인메모리 구현체.
type HeartbeatAdapter struct {
	mu       sync.RWMutex
	nodes    map[string]*NodeHealth
	phiMap   map[string]*phiState
	handlers []func(failedNodeID string, phi float64)
	bus      EventBusPort
}

// 컴파일 타임 인터페이스 계약 검증.
var _ HeartbeatPort = (*HeartbeatAdapter)(nil)

// NewHeartbeatAdapter — HeartbeatAdapter 생성.
func NewHeartbeatAdapter(bus EventBusPort) *HeartbeatAdapter {
	return &HeartbeatAdapter{
		nodes:  make(map[string]*NodeHealth),
		phiMap: make(map[string]*phiState),
		bus:    bus,
	}
}

func (a *HeartbeatAdapter) Beat(ctx context.Context, nodeID string, role NodeRole) error {
	// 빈 nodeID 검증.
	if nodeID == "" {
		return fmt.Errorf("heartbeat: nodeID is required")
	}
	// 유효하지 않은 NodeRole 검증.
	if !validNodeRoles[role] {
		return fmt.Errorf("heartbeat: invalid NodeRole %q", role)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	now := time.Now()
	ps, ok := a.phiMap[nodeID]
	if !ok {
		ps = &phiState{}
		a.phiMap[nodeID] = ps
	}
	if !ps.lastSeen.IsZero() {
		interval := now.Sub(ps.lastSeen)
		ps.intervals = append(ps.intervals, interval)
		if len(ps.intervals) > 20 { // 최근 20개만 유지.
			ps.intervals = ps.intervals[1:]
		}
	}
	ps.lastSeen = now

	a.nodes[nodeID] = &NodeHealth{
		NodeID:   nodeID,
		Role:     role,
		IsAlive:  true,
		LastSeen: now,
		PhiScore: ps.phi(),
	}
	return nil
}

func (a *HeartbeatAdapter) GetHealth(ctx context.Context, nodeID string) (*NodeHealth, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	h, ok := a.nodes[nodeID]
	if !ok {
		return nil, fmt.Errorf("heartbeat: node not found: %s", nodeID)
	}
	copy := *h
	return &copy, nil
}

func (a *HeartbeatAdapter) ListAllNodes(ctx context.Context) ([]NodeHealth, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	var result []NodeHealth
	for _, h := range a.nodes {
		result = append(result, *h)
	}
	return result, nil
}

func (a *HeartbeatAdapter) WatchFailure(ctx context.Context, handler func(failedNodeID string, phi float64)) error {
	a.mu.Lock()
	a.handlers = append(a.handlers, handler)
	a.mu.Unlock()
	return nil
}

// CheckAndNotifyFailures — 장애 감지 루프 (FailoverBot이 주기적으로 호출).
// Phi > 8.0 이면 장애로 판단.
func (a *HeartbeatAdapter) CheckAndNotifyFailures(ctx context.Context) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for nodeID, ps := range a.phiMap {
		phi := ps.phi()
		if phi > 8.0 {
			for _, h := range a.handlers {
				h(nodeID, phi)
			}
		}
	}
}

// ─── FailoverAdapter ─────────────────────────────────────────────────────

// FailoverAdapter — FailoverPort 인메모리 구현체.
type FailoverAdapter struct {
	mu        sync.RWMutex
	history   []FailoverEvent
	fenced    map[string]bool // 펜싱된 노드 ID
	activeVIP string          // 현재 VIP가 가리키는 노드 ID
	bus       EventBusPort
}

// 컴파일 타임 인터페이스 계약 검증.
var _ FailoverPort = (*FailoverAdapter)(nil)

// NewFailoverAdapter — FailoverAdapter 생성.
func NewFailoverAdapter(bus EventBusPort) *FailoverAdapter {
	return &FailoverAdapter{
		fenced: make(map[string]bool),
		bus:    bus,
	}
}

func (a *FailoverAdapter) TriggerFailover(ctx context.Context, failedNodeID string, reason FailoverReason) (*FailoverEvent, error) {
	// 빈 failedNodeID 검증.
	if failedNodeID == "" {
		return nil, fmt.Errorf("failover: failedNodeID is required")
	}

	// 1. Fencing 먼저 실행 (STONITH).
	if err := a.FenceNode(ctx, failedNodeID); err != nil {
		return nil, fmt.Errorf("failover: fencing failed: %w", err)
	}

	evt := &FailoverEvent{
		EventID:      newID("fo"),
		FailedNodeID: failedNodeID,
		NewActiveID:  "node-b", // 실제: Witness 투표 결과로 결정
		Reason:       reason,
		FencingDone:  true,
		OccurredAt:   time.Now(),
	}

	a.mu.Lock()
	a.history = append(a.history, *evt)
	a.mu.Unlock()

	// NATS 이벤트 발행.
	_ = a.bus.Publish(ctx, DomainEvent{
		EventID:    newID("evt"),
		Subject:    "sovereign.ha.failover.triggered",
		OrgID:      "",
		Payload:    []byte(fmt.Sprintf(`{"failed_node":%q,"new_active":%q,"reason":%q}`, failedNodeID, evt.NewActiveID, reason)),
		OccurredAt: time.Now(),
	})

	return evt, nil
}

func (a *FailoverAdapter) FenceNode(ctx context.Context, nodeID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fenced[nodeID] = true
	return nil
}

func (a *FailoverAdapter) PromoteStandby(ctx context.Context, nodeID string) error {
	// 실제: Neon Compute 시작 + Safekeeper 라우팅 전환.
	return nil
}

func (a *FailoverAdapter) SwitchVIP(ctx context.Context, newActiveNodeID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.activeVIP = newActiveNodeID
	return nil
}

func (a *FailoverAdapter) GetFailoverHistory(ctx context.Context, limit int) ([]FailoverEvent, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	// limit=0 → 빈 결과 반환 (명시적 요청).
	if limit == 0 {
		return []FailoverEvent{}, nil
	}
	// limit 음수 → 에러.
	if limit < 0 {
		return nil, fmt.Errorf("failover: limit must be >= 0, got %d", limit)
	}
	if limit > len(a.history) {
		limit = len(a.history)
	}
	result := make([]FailoverEvent, limit)
	copy(result, a.history[len(a.history)-limit:])
	return result, nil
}

// IsFenced — 특정 노드가 펜싱됐는지 확인.
func (a *FailoverAdapter) IsFenced(nodeID string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.fenced[nodeID]
}

// ─── BackupAdapter ────────────────────────────────────────────────────────

// BackupAdapter — BackupPort 인메모리 구현체.
type BackupAdapter struct {
	mu      sync.RWMutex
	backups map[string]*BackupRecord // backupID → record
	walLag  map[string]int64         // nodeID → WAL lag bytes
	bus     EventBusPort
}

// 컴파일 타임 인터페이스 계약 검증.
var _ BackupPort = (*BackupAdapter)(nil)

// NewBackupAdapter — BackupAdapter 생성.
func NewBackupAdapter(bus EventBusPort) *BackupAdapter {
	return &BackupAdapter{
		backups: make(map[string]*BackupRecord),
		walLag:  make(map[string]int64),
		bus:     bus,
	}
}

func (a *BackupAdapter) TriggerSnapshot(ctx context.Context, backupType BackupType) (*BackupRecord, error) {
	backupID := newID("bak")
	rec := &BackupRecord{
		BackupID:   backupID,
		Type:       backupType,
		SourceNode: "node-a",
		DestNode:   "node-b-minio",
		SizeBytes:  0, // 실제: Velero/WAL 스냅샷 크기
		CreatedAt:  time.Now(),
		ExpiresAt:  time.Now().Add(30 * 24 * time.Hour), // 30일 보존
	}

	a.mu.Lock()
	a.backups[backupID] = rec
	a.mu.Unlock()

	_ = a.bus.Publish(ctx, DomainEvent{
		EventID:    newID("evt"),
		Subject:    "sovereign.ha.backup.created",
		OrgID:      "",
		Payload:    []byte(fmt.Sprintf(`{"backup_id":%q,"type":%q}`, backupID, backupType)),
		OccurredAt: time.Now(),
	})

	return rec, nil
}

func (a *BackupAdapter) ListBackups(ctx context.Context, backupType BackupType, limit int) ([]BackupRecord, error) {
	// limit=0 → 빈 결과 반환.
	if limit == 0 {
		return []BackupRecord{}, nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	var result []BackupRecord
	for _, rec := range a.backups {
		if backupType != "" && rec.Type != backupType {
			continue
		}
		result = append(result, *rec)
		if limit > 0 && len(result) >= limit {
			break
		}
	}
	return result, nil
}

func (a *BackupAdapter) RestoreFromBackup(ctx context.Context, backupID, targetNodeID string) error {
	// 빈 targetNodeID 검증.
	if targetNodeID == "" {
		return fmt.Errorf("backup: targetNodeID is required")
	}
	a.mu.RLock()
	_, ok := a.backups[backupID]
	a.mu.RUnlock()
	if !ok {
		return fmt.Errorf("backup: not found: %s", backupID)
	}
	// 실제: Velero restore + WAL catch-up 실행.
	return nil
}

func (a *BackupAdapter) GetWALLag(ctx context.Context, standbyNodeID string) (int64, error) {
	// 빈 standbyNodeID 검증.
	if standbyNodeID == "" {
		return 0, fmt.Errorf("backup: standbyNodeID is required")
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	lag, ok := a.walLag[standbyNodeID]
	if !ok {
		return 0, nil // 동기화됨
	}
	return lag, nil
}

func (a *BackupAdapter) PruneOldBackups(ctx context.Context, retentionDays int) (int, error) {
	// 음수 보존 기간 검증.
	if retentionDays < 0 {
		return 0, fmt.Errorf("backup: retentionDays must be >= 0, got %d", retentionDays)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	cutoff := time.Now().AddDate(0, 0, -retentionDays)
	deleted := 0
	for id, rec := range a.backups {
		if rec.CreatedAt.Before(cutoff) {
			delete(a.backups, id)
			deleted++
		}
	}
	return deleted, nil
}

// SetWALLag — 테스트용: WAL 지연 설정.
func (a *BackupAdapter) SetWALLag(nodeID string, lagBytes int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.walLag[nodeID] = lagBytes
}
