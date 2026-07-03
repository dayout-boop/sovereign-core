package main

import (
	"context"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────
// ha_ports.go — 물리 격리 환경 2노드 HA 포트 정의
//
// 설계 결정 (HA_BACKUP_RECOVERY_DESIGN.md 기반):
//   - 2노드 + Witness Node 구조로 Split-Brain 방지
//   - WAL 스트리밍 기반 실시간 DB 복제
//   - Velero + 로컬 MinIO 기반 K8s 상태 백업
//   - NATS 이벤트 기반 자동 페일오버 파이프라인
// ─────────────────────────────────────────────────────────────────────────

// ─── 노드 상태 정의 ──────────────────────────────────────────────────────

// NodeRole — 노드 역할.
type NodeRole string

const (
	NodeRoleActive   NodeRole = "active"   // 쓰기 허용 노드
	NodeRoleStandby  NodeRole = "standby"  // 읽기 전용 대기 노드
	NodeRoleWitness  NodeRole = "witness"  // 투표 전용 (데이터 없음)
	NodeRoleUnknown  NodeRole = "unknown"  // 상태 불명
)

// NodeHealth — 노드 헬스 상태.
type NodeHealth struct {
	NodeID      string    `json:"node_id"`
	Role        NodeRole  `json:"role"`
	IsAlive     bool      `json:"is_alive"`
	LastSeen    time.Time `json:"last_seen"`
	WALLag      int64     `json:"wal_lag_bytes"` // Standby의 WAL 지연 (0=동기화됨)
	PhiScore    float64   `json:"phi_score"`     // Phi-accrual 장애 점수 (>8=장애)
}

// ─── Heartbeat 포트 ──────────────────────────────────────────────────────

// HeartbeatPort — 노드 간 상태 모니터링 경계.
// 설계 원칙:
//   - Phi-accrual 장애 감지기(neon_engine 패치 기반)와 연동.
//   - 장애 감지 시 NATS 이벤트 발행 → FailoverBot이 수신.
//   - Witness Node 포함 3노드 모두 HeartbeatPort를 구현.
type HeartbeatPort interface {
	// Beat — 현재 노드의 생존 신호 발송.
	Beat(ctx context.Context, nodeID string, role NodeRole) error
	// GetHealth — 특정 노드의 헬스 상태 조회.
	GetHealth(ctx context.Context, nodeID string) (*NodeHealth, error)
	// ListAllNodes — 클러스터 전체 노드 상태 조회.
	ListAllNodes(ctx context.Context) ([]NodeHealth, error)
	// WatchFailure — 장애 감지 시 콜백 등록. 비동기 감시.
	WatchFailure(ctx context.Context, handler func(failedNodeID string, phi float64)) error
}

// ─── 페일오버 포트 ───────────────────────────────────────────────────────

// FailoverReason — 페일오버 발생 원인.
type FailoverReason string

const (
	FailoverReasonHeartbeatTimeout FailoverReason = "heartbeat_timeout" // 하트비트 타임아웃
	FailoverReasonManual           FailoverReason = "manual"            // 운영자 수동 전환
	FailoverReasonWALLagExceeded   FailoverReason = "wal_lag_exceeded"  // WAL 지연 임계값 초과
)

// FailoverEvent — 페일오버 이벤트 기록.
type FailoverEvent struct {
	EventID      string         `json:"event_id"`
	FailedNodeID string         `json:"failed_node_id"`
	NewActiveID  string         `json:"new_active_id"`
	Reason       FailoverReason `json:"reason"`
	FencingDone  bool           `json:"fencing_done"` // STONITH 완료 여부
	OccurredAt   time.Time      `json:"occurred_at"`
	CompletedAt  time.Time      `json:"completed_at,omitempty"`
}

// FailoverPort — 자동 페일오버 및 Fencing 경계.
// 설계 원칙:
//   - Quorum(Witness 포함) 확인 후에만 페일오버 실행.
//   - Fencing(STONITH) 완료 후에만 신규 Active 승격.
//   - 페일오버 이력은 NATS 스트림에 불변 기록.
type FailoverPort interface {
	// TriggerFailover — 페일오버 시작. Quorum 확인 → Fencing → Active 승격.
	TriggerFailover(ctx context.Context, failedNodeID string, reason FailoverReason) (*FailoverEvent, error)
	// FenceNode — STONITH: 장애 노드 네트워크/전원 차단.
	FenceNode(ctx context.Context, nodeID string) error
	// PromoteStandby — Standby 노드를 Active로 승격.
	PromoteStandby(ctx context.Context, nodeID string) error
	// SwitchVIP — VIP(Virtual IP)를 신규 Active 노드로 전환.
	SwitchVIP(ctx context.Context, newActiveNodeID string) error
	// GetFailoverHistory — 페일오버 이력 조회.
	GetFailoverHistory(ctx context.Context, limit int) ([]FailoverEvent, error)
}

// ─── 백업 포트 ───────────────────────────────────────────────────────────

// BackupType — 백업 유형.
type BackupType string

const (
	BackupTypeWAL      BackupType = "wal"       // WAL 스트리밍 연속 백업
	BackupTypeSnapshot BackupType = "snapshot"  // 전체 스냅샷 (Velero)
	BackupTypeEtcd     BackupType = "etcd"      // etcd 스냅샷
)

// BackupRecord — 백업 기록.
type BackupRecord struct {
	BackupID    string     `json:"backup_id"`
	Type        BackupType `json:"type"`
	SourceNode  string     `json:"source_node"`
	DestNode    string     `json:"dest_node"`   // 로컬 MinIO 노드
	SizeBytes   int64      `json:"size_bytes"`
	WALPosition string     `json:"wal_position,omitempty"` // WAL LSN
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
}

// BackupPort — 백업 스케줄링 및 복원 경계.
// 설계 원칙:
//   - 외부 클라우드 스토리지 의존 없음. 로컬 MinIO(S3 호환) 사용.
//   - WAL 연속 백업 + 주기적 스냅샷(매시간) 이중 보호.
//   - 복원 시 WAL Catch-up으로 RPO(복구 목표 시점) 최소화.
type BackupPort interface {
	// TriggerSnapshot — 즉시 스냅샷 백업 실행 (Velero 연동).
	TriggerSnapshot(ctx context.Context, backupType BackupType) (*BackupRecord, error)
	// ListBackups — 백업 목록 조회.
	ListBackups(ctx context.Context, backupType BackupType, limit int) ([]BackupRecord, error)
	// RestoreFromBackup — 특정 백업에서 복원.
	RestoreFromBackup(ctx context.Context, backupID, targetNodeID string) error
	// GetWALLag — Standby 노드의 WAL 지연 바이트 조회.
	GetWALLag(ctx context.Context, standbyNodeID string) (lagBytes int64, err error)
	// PruneOldBackups — 보존 기간 초과 백업 삭제.
	PruneOldBackups(ctx context.Context, retentionDays int) (deletedCount int, err error)
}
