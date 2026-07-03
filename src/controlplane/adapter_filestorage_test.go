package main

// ─────────────────────────────────────────────────────────────────────────
// T6 — 실물 StoragePort 어댑터(FileStorage) 통합 검증.
//
// 목적: StoragePort 계약이 mock 이 아니라 "실제 디스크 I/O·영속·동시성"
// 환경에서도 지켜지는지 증명. 초기 부트스트랩 트랙의 첫 실물 배선.
//
// 검증 명제:
//   T6-A 인터페이스 만족: FileStorage 가 StoragePort 로 대입 가능(컴파일)
//   T6-B 영속·복원: 쓰고 → 재기동(재로드) → 상태가 그대로 살아있다
//   T6-C 동시성 안전: 동시 CreateBranch N 개 → 정확히 N 개 영속(손실/중복 0)
//   T6-D 웜풀 연동: 실물 어댑터로 WarmPool 히트/미스가 정상 동작
//   T6-E RLS: 남의 org 브랜치 삭제 차단
// ─────────────────────────────────────────────────────────────────────────

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// T6-A: 컴파일 타임 인터페이스 만족 확인.
var _ StoragePort = (*FileStorage)(nil)

func TestStage2_T6A_InterfaceSatisfied(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStorage(filepath.Join(dir, "state.json"), "syd", 150)
	if err != nil {
		t.Fatalf("NewFileStorage: %v", err)
	}
	var _ StoragePort = fs // 실물 어댑터를 포트로 사용 가능
	t.Log("[T6-A] FileStorage 가 StoragePort 계약을 만족(실물 교체 가능)")
}

func TestStage2_T6B_PersistAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	ctx := context.Background()

	fs, _ := NewFileStorage(path, "syd", 150)
	brID, err := fs.CreateBranch(ctx, "orgA", "proj1", "")
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	instID, _, _, err := fs.BootInstance(ctx, "syd")
	if err != nil {
		t.Fatalf("BootInstance: %v", err)
	}
	if err := fs.AttachBranch(ctx, instID, brID); err != nil {
		t.Fatalf("AttachBranch: %v", err)
	}

	// 재기동 시뮬: 완전히 새 인스턴스로 디스크에서 재로드.
	reloaded, err := NewFileStorage(path, "syd", 150)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	b, ok := reloaded.state.Branches[brID]
	if !ok || b.OrgID != "orgA" {
		t.Fatalf("[T6-B] 재기동 후 브랜치 유실/손상: ok=%v %+v", ok, b)
	}
	inst, ok := reloaded.state.Instances[instID]
	if !ok || inst.BranchID != brID {
		t.Fatalf("[T6-B] 재기동 후 인스턴스-브랜치 연결 유실: ok=%v %+v", ok, inst)
	}
	t.Logf("[T6-B] 영속·복원 정상: branch=%s inst=%s 재기동 후에도 유지", brID, instID)
}

func TestStage2_T6C_ConcurrentPersist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	ctx := context.Background()
	fs, _ := NewFileStorage(path, "syd", 150)

	const N = 100
	var wg sync.WaitGroup
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, err := fs.CreateBranch(ctx, "orgA", fmt.Sprintf("proj%d", n), "")
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("[T6-C] 동시 생성 중 오류: %v", e)
	}

	// 디스크에서 재로드해 실제 영속 개수 확인.
	branches, _, err := fs.reloadCounts()
	if err != nil {
		t.Fatalf("reloadCounts: %v", err)
	}
	if branches != N {
		t.Fatalf("[T6-C 결함] 동시 %d 생성인데 디스크엔 %d 개만 영속 → 손실/경합", N, branches)
	}
	t.Logf("[T6-C] 동시 %d 생성 → 디스크 영속 정확히 %d (손실/중복 0)", N, branches)
}

func TestStage2_T6D_WarmPoolWithRealAdapter(t *testing.T) {
	dir := t.TempDir()
	fs, _ := NewFileStorage(filepath.Join(dir, "state.json"), "syd", 150)
	ctx := context.Background()

	// 실물 어댑터로 웜풀 구동(고정 3개 예열).
	pool := NewWarmPool(fs, "syd", FixedPolicy{N: 3})
	ready, _, _, _ := pool.Stats()
	if ready < 1 {
		t.Fatalf("[T6-D] 실물 어댑터로 예열 실패: ready=%d", ready)
	}
	// 히트: 예열분에서 꺼냄 → boot 0
	_, hit, bootMS, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if !hit || bootMS != 0 {
		t.Fatalf("[T6-D] 예열분 히트 기대인데 hit=%v boot=%d", hit, bootMS)
	}
	// 드레인 후 미스: 콜드부팅 실측값(150)
	pool.Drain()
	_, hit2, bootMS2, _ := pool.Acquire(ctx)
	if hit2 || bootMS2 != 150 {
		t.Fatalf("[T6-D] 드레인 후 미스 기대인데 hit=%v boot=%d", hit2, bootMS2)
	}
	t.Logf("[T6-D] 실물 어댑터 웜풀 정상: 히트 boot=0 / 드레인후 미스 boot=%dms", bootMS2)
}

func TestStage2_T6E_RLSOnDelete(t *testing.T) {
	dir := t.TempDir()
	fs, _ := NewFileStorage(filepath.Join(dir, "state.json"), "syd", 150)
	ctx := context.Background()

	brID, _ := fs.CreateBranch(ctx, "orgA", "proj1", "")
	// 다른 org 가 삭제 시도 → 차단되어야 함.
	if err := fs.DeleteBranch(ctx, "orgB", brID); err == nil {
		t.Fatalf("[T6-E 결함] orgB 가 orgA 의 브랜치를 삭제 성공 → RLS 위반")
	}
	// 정당한 owner 는 성공.
	if err := fs.DeleteBranch(ctx, "orgA", brID); err != nil {
		t.Fatalf("[T6-E] 정당 소유자 삭제 실패: %v", err)
	}
	t.Log("[T6-E] 실물 어댑터에서도 교차테넌트 삭제 차단(RLS) 유지")
}
