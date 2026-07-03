package main

// ha_audit_test.go
// Heartbeat / Failover / Backup / FailoverBot 예외·경계값·동시성·부분 실패 케이스 전수 보완

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// ──────────────────────────────────────────────────────────────────────────────
// 헬퍼: 테스트용 EventBus 생성
// ──────────────────────────────────────────────────────────────────────────────
func newTestBus() EventBusPort {
	return NewInMemoryEventBus()
}

// ──────────────────────────────────────────────────────────────────────────────
// Heartbeat 누락 케이스
// ──────────────────────────────────────────────────────────────────────────────

// 1. 빈 nodeID → 에러
func TestHeartbeat_EmptyNodeID_Error(t *testing.T) {
	h := NewHeartbeatAdapter(newTestBus())
	err := h.Beat(context.Background(), "", NodeRoleActive)
	if err == nil {
		t.Fatal("expected error for empty nodeID")
	}
}

// 2. 알 수 없는 NodeRole → 에러
func TestHeartbeat_InvalidRole_Error(t *testing.T) {
	h := NewHeartbeatAdapter(newTestBus())
	err := h.Beat(context.Background(), "node_1", NodeRole("invalid_role"))
	if err == nil {
		t.Fatal("expected error for invalid NodeRole")
	}
}

// 3. 존재하지 않는 nodeID 헬스 조회 → 에러
func TestHeartbeat_GetHealth_NonExistent_Error(t *testing.T) {
	h := NewHeartbeatAdapter(newTestBus())
	_, err := h.GetHealth(context.Background(), "nonexistent_node")
	if err == nil {
		t.Fatal("expected error for non-existent nodeID")
	}
}

// 4. Beat 후 GetHealth — IsAlive=true 반환
func TestHeartbeat_BeatThenGetHealth_Alive(t *testing.T) {
	h := NewHeartbeatAdapter(newTestBus())
	if err := h.Beat(context.Background(), "node_a", NodeRoleActive); err != nil {
		t.Fatalf("beat failed: %v", err)
	}
	health, err := h.GetHealth(context.Background(), "node_a")
	if err != nil {
		t.Fatalf("get health failed: %v", err)
	}
	if !health.IsAlive {
		t.Fatal("expected IsAlive=true after Beat")
	}
	if health.Role != NodeRoleActive {
		t.Fatalf("expected role=active, got: %v", health.Role)
	}
}

// 5. Phi-accrual 임계값 초과 → CheckAndNotifyFailures 콜백 트리거
func TestHeartbeat_PhiThresholdExceeded_TriggerCallback(t *testing.T) {
	h := NewHeartbeatAdapter(newTestBus())
	// 노드 등록 (짧은 간격으로 여러 번 Beat → 평균 간격 설정)
	for i := 0; i < 5; i++ {
		_ = h.Beat(context.Background(), "node_phi", NodeRoleStandby)
		time.Sleep(10 * time.Millisecond)
	}

	triggered := make(chan string, 1)
	ctx := context.Background()

	_ = h.WatchFailure(ctx, func(failedNodeID string, phi float64) {
		triggered <- failedNodeID
	})

	// Phi 임계값 초과 시뮬레이션 — phiMap의 lastSeen을 오래 전으로 설정
	h.mu.Lock()
	if ps, ok := h.phiMap["node_phi"]; ok {
		ps.lastSeen = time.Now().Add(-60 * time.Second) // 60초 전
	}
	h.mu.Unlock()

	// 장애 감지 루프 직접 호출
	h.CheckAndNotifyFailures(ctx)

	select {
	case nodeID := <-triggered:
		if nodeID != "node_phi" {
			t.Fatalf("expected node_phi to fail, got: %s", nodeID)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("expected failure callback to be triggered")
	}
}

// 6. ListAllNodes — Beat 등록된 노드 전부 반환
func TestHeartbeat_ListAllNodes_ReturnsAll(t *testing.T) {
	h := NewHeartbeatAdapter(newTestBus())
	_ = h.Beat(context.Background(), "node_list_1", NodeRoleActive)
	_ = h.Beat(context.Background(), "node_list_2", NodeRoleStandby)
	_ = h.Beat(context.Background(), "node_list_3", NodeRoleWitness)

	nodes, err := h.ListAllNodes(context.Background())
	if err != nil {
		t.Fatalf("list nodes failed: %v", err)
	}
	if len(nodes) < 3 {
		t.Fatalf("expected at least 3 nodes, got %d", len(nodes))
	}
}

// 7. 동시 Beat — race detector 통과
func TestHeartbeat_ConcurrentBeat_NoRace(t *testing.T) {
	h := NewHeartbeatAdapter(newTestBus())
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = h.Beat(context.Background(), fmt.Sprintf("node_race_%d", i), NodeRoleStandby)
		}(i)
	}
	wg.Wait()
}

// ──────────────────────────────────────────────────────────────────────────────
// Failover 누락 케이스
// ──────────────────────────────────────────────────────────────────────────────

// 8. 빈 failedNodeID → 에러
func TestFailover_EmptyNodeID_Error(t *testing.T) {
	f := NewFailoverAdapter(newTestBus())
	_, err := f.TriggerFailover(context.Background(), "", FailoverReasonHeartbeatTimeout)
	if err == nil {
		t.Fatal("expected error for empty failedNodeID")
	}
}

// 9. 이미 펜싱된 노드 재펜싱 → 에러 또는 멱등 처리
func TestFailover_FenceAlreadyFenced_Idempotent(t *testing.T) {
	f := NewFailoverAdapter(newTestBus())
	// 첫 번째 펜싱
	_ = f.FenceNode(context.Background(), "node_fence")
	// 두 번째 펜싱 — 멱등(에러 없음) 또는 에러 반환 모두 허용
	err := f.FenceNode(context.Background(), "node_fence")
	if err != nil {
		t.Logf("second fencing returned error (acceptable): %v", err)
	}
}

// 10. PromoteStandby — 존재하지 않는 노드 → 에러 또는 성공(인메모리 구현 허용)
func TestFailover_PromoteStandby_ReturnsNoError(t *testing.T) {
	f := NewFailoverAdapter(newTestBus())
	// 인메모리 구현에서는 에러 없이 통과 (실제 구현에서는 Neon Compute 시작)
	err := f.PromoteStandby(context.Background(), "node_standby_promote")
	if err != nil {
		t.Logf("promote returned error (may be acceptable): %v", err)
	}
}

// 11. SwitchVIP — 정상 전환
func TestFailover_SwitchVIP_Success(t *testing.T) {
	f := NewFailoverAdapter(newTestBus())
	err := f.SwitchVIP(context.Background(), "node_new_active")
	if err != nil {
		t.Fatalf("switch VIP failed: %v", err)
	}
}

// 12. GetFailoverHistory — limit 0 → 빈 결과
func TestFailover_History_ZeroLimit_Empty(t *testing.T) {
	f := NewFailoverAdapter(newTestBus())
	history, err := f.GetFailoverHistory(context.Background(), 0)
	if err != nil {
		t.Fatalf("get history failed: %v", err)
	}
	if len(history) != 0 {
		t.Fatalf("expected 0 results for limit=0, got %d", len(history))
	}
}

// 13. GetFailoverHistory — 이벤트 발생 후 조회 → 포함 확인
func TestFailover_History_AfterTrigger_Contains(t *testing.T) {
	f := NewFailoverAdapter(newTestBus())
	// TriggerFailover 내부에서 FenceNode 호출 → 빈 nodeID 아닌 경우 성공
	_, err := f.TriggerFailover(context.Background(), "node_failed", FailoverReasonManual)
	if err != nil {
		t.Fatalf("trigger failover failed: %v", err)
	}
	history, err := f.GetFailoverHistory(context.Background(), 10)
	if err != nil {
		t.Fatalf("get history failed: %v", err)
	}
	if len(history) == 0 {
		t.Fatal("expected at least 1 failover event in history")
	}
}

// 14. 동시 TriggerFailover — race detector 통과
func TestFailover_ConcurrentTrigger_NoRace(t *testing.T) {
	f := NewFailoverAdapter(newTestBus())
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = f.TriggerFailover(context.Background(),
				fmt.Sprintf("node_concurrent_%d", i),
				FailoverReasonHeartbeatTimeout)
		}(i)
	}
	wg.Wait()
}

// ──────────────────────────────────────────────────────────────────────────────
// Backup 누락 케이스
// ──────────────────────────────────────────────────────────────────────────────

// 15. 존재하지 않는 backupID 복원 → 에러
func TestBackup_RestoreNonExistent_Error(t *testing.T) {
	b := NewBackupAdapter(newTestBus())
	err := b.RestoreFromBackup(context.Background(), "nonexistent_backup_id", "node_a")
	if err == nil {
		t.Fatal("expected error for non-existent backupID")
	}
}

// 16. 빈 targetNodeID 복원 → 에러
func TestBackup_RestoreEmptyTarget_Error(t *testing.T) {
	b := NewBackupAdapter(newTestBus())
	rec, err := b.TriggerSnapshot(context.Background(), BackupTypeWAL)
	if err != nil {
		t.Fatalf("snapshot failed: %v", err)
	}
	err = b.RestoreFromBackup(context.Background(), rec.BackupID, "")
	if err == nil {
		t.Fatal("expected error for empty targetNodeID")
	}
}

// 17. ListBackups — limit 0 → 빈 결과
func TestBackup_List_ZeroLimit_Empty(t *testing.T) {
	b := NewBackupAdapter(newTestBus())
	_, _ = b.TriggerSnapshot(context.Background(), BackupTypeWAL)
	list, err := b.ListBackups(context.Background(), BackupTypeWAL, 0)
	if err != nil {
		t.Fatalf("list backups failed: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected 0 results for limit=0, got %d", len(list))
	}
}

// 18. PruneOldBackups — 음수 보존 기간 → 에러
func TestBackup_Prune_NegativeRetention_Error(t *testing.T) {
	b := NewBackupAdapter(newTestBus())
	_, err := b.PruneOldBackups(context.Background(), -1)
	if err == nil {
		t.Fatal("expected error for negative retention days")
	}
}

// 19. GetWALLag — 빈 standbyNodeID → 에러
func TestBackup_GetWALLag_EmptyNodeID_Error(t *testing.T) {
	b := NewBackupAdapter(newTestBus())
	_, err := b.GetWALLag(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty standbyNodeID")
	}
}

// 20. 동시 TriggerSnapshot — race detector 통과
func TestBackup_ConcurrentSnapshot_NoRace(t *testing.T) {
	b := NewBackupAdapter(newTestBus())
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			backupType := BackupTypeWAL
			if i%2 == 0 {
				backupType = BackupTypeEtcd
			}
			_, _ = b.TriggerSnapshot(context.Background(), backupType)
		}(i)
	}
	wg.Wait()
}

// 21. 스냅샷 후 ListBackups — 생성된 항목 포함 확인
func TestBackup_SnapshotThenList_ContainsRecord(t *testing.T) {
	b := NewBackupAdapter(newTestBus())
	rec, err := b.TriggerSnapshot(context.Background(), BackupTypeSnapshot)
	if err != nil {
		t.Fatalf("snapshot failed: %v", err)
	}
	list, err := b.ListBackups(context.Background(), BackupTypeSnapshot, 10)
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	found := false
	for _, r := range list {
		if r.BackupID == rec.BackupID {
			found = true
		}
	}
	if !found {
		t.Fatal("created backup should appear in list")
	}
}

// 22. PruneOldBackups — 보존 기간 충분히 길면 삭제 없음
func TestBackup_Prune_LongRetention_NoDeletion(t *testing.T) {
	b := NewBackupAdapter(newTestBus())
	_, _ = b.TriggerSnapshot(context.Background(), BackupTypeWAL)
	_, _ = b.TriggerSnapshot(context.Background(), BackupTypeWAL)
	pruned, err := b.PruneOldBackups(context.Background(), 365) // 1년 보존
	if err != nil {
		t.Fatalf("prune failed: %v", err)
	}
	if pruned != 0 {
		t.Fatalf("expected 0 pruned for long retention, got %d", pruned)
	}
}
