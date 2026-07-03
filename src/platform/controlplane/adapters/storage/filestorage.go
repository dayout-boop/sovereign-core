package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────
// FileStorage — StoragePort 의 "실물" 어댑터(초기 부트스트랩 트랙).
//
// MockStorage 와 달리 메모리에서 성공만 반환하지 않고, 실제 디스크에 상태를
// JSON 으로 영속한다. 즉 "mock 이 아니라 진짜 외부 영속 엔진과 대화하는" 첫 단계다.
//
// 설계 의도(이원화 로드맵과의 연결):
//   - 지금: 로컬 디스크(JSON)에 브랜치/인스턴스/엔드포인트 메타를 영속.
//   - 초기 트랙 전개: 아래 save/load 만 Vultr 관리형 Postgres 드라이버로 교체하면
//     제어평면 로직/포트 계약은 그대로 둔 채 실물 DB 로 승격 가능.
//   - 최종 트랙: BootInstance 를 자체 Firecracker 부팅으로 교체.
//
// 이 어댑터가 증명하려는 것: StoragePort 계약이 mock 뿐 아니라 실제 I/O·영속·
// 동시성 환경에서도 지켜지는가(직렬화, 잠금, 재기동 후 상태 복원).
// ─────────────────────────────────────────────────────────────────────────

type fsState struct {
	Branches  map[string]fsBranch   `json:"branches"`
	Instances map[string]fsInstance `json:"instances"`
	Endpoints map[string]bool       `json:"endpoints"` // true=active, false=suspended
}

type fsBranch struct {
	OrgID     string `json:"org_id"`
	ProjectID string `json:"project_id"`
	Parent    string `json:"parent"`
	CreatedAt string `json:"created_at"`
}

type fsInstance struct {
	Host     string `json:"host"`
	Region   string `json:"region"`
	BranchID string `json:"branch_id"`
}

// FileStorage 는 디스크 JSON 파일에 상태를 영속하는 StoragePort 구현이다.
type FileStorage struct {
	mu         sync.Mutex
	path       string
	region     string
	coldBootMS int64 // 진짜 Firecracker 전까지 콜드부팅 시뮬값
	state      fsState
}

// NewFileStorage 는 path 의 기존 상태를 로드(있으면)하고 어댑터를 만든다.
func NewFileStorage(path, region string, coldBootMS int64) (*FileStorage, error) {
	fs := &FileStorage{
		path:       path,
		region:     region,
		coldBootMS: coldBootMS,
		state: fsState{
			Branches:  map[string]fsBranch{},
			Instances: map[string]fsInstance{},
			Endpoints: map[string]bool{},
		},
	}
	if err := fs.load(); err != nil {
		return nil, err
	}
	return fs, nil
}

func (fs *FileStorage) load() error {
	b, err := os.ReadFile(fs.path)
	if os.IsNotExist(err) {
		return nil // 신규 — 빈 상태로 시작
	}
	if err != nil {
		return fmt.Errorf("filestorage load: %w", err)
	}
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, &fs.state)
}

// saveLocked 는 원자적 쓰기(임시파일 → rename)로 부분쓰기/손상을 방지한다.
// 호출자가 fs.mu 를 보유해야 한다.
func (fs *FileStorage) saveLocked() error {
	b, err := json.MarshalIndent(fs.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := fs.path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(fs.path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, fs.path) // POSIX 원자적 교체
}

func (fs *FileStorage) CreateBranch(_ context.Context, orgID, projectID, parentBranchID string) (string, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	id := newID("br")
	fs.state.Branches[id] = fsBranch{
		OrgID: orgID, ProjectID: projectID, Parent: parentBranchID,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := fs.saveLocked(); err != nil {
		delete(fs.state.Branches, id) // 영속 실패 시 인메모리도 롤백(정합성)
		return "", err
	}
	return id, nil
}

func (fs *FileStorage) DeleteBranch(_ context.Context, orgID, branchID string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	b, ok := fs.state.Branches[branchID]
	if !ok || b.OrgID != orgID { // RLS: 남의 브랜치 삭제 금지
		return fmt.Errorf("branch not found for org")
	}
	delete(fs.state.Branches, branchID)
	return fs.saveLocked()
}

func (fs *FileStorage) BootInstance(_ context.Context, region string) (string, string, int64, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	id := newID("inst")
	host := fmt.Sprintf("%s.%s.compute.internal", id, region)
	fs.state.Instances[id] = fsInstance{Host: host, Region: region}
	if err := fs.saveLocked(); err != nil {
		delete(fs.state.Instances, id)
		return "", "", fs.coldBootMS, err
	}
	return id, host, fs.coldBootMS, nil
}

func (fs *FileStorage) AttachBranch(_ context.Context, instanceID, branchID string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	inst, ok := fs.state.Instances[instanceID]
	if !ok {
		return fmt.Errorf("instance not found")
	}
	inst.BranchID = branchID
	fs.state.Instances[instanceID] = inst
	return fs.saveLocked()
}

func (fs *FileStorage) SuspendEndpoint(_ context.Context, _, endpointID string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.state.Endpoints[endpointID] = false // scale-to-zero
	return fs.saveLocked()
}

func (fs *FileStorage) DeleteEndpoint(_ context.Context, _, endpointID string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	delete(fs.state.Endpoints, endpointID)
	return fs.saveLocked()
}

// 관측용(테스트): 영속된 브랜치/인스턴스 수를 디스크에서 다시 읽어 반환.
func (fs *FileStorage) reloadCounts() (branches, instances int, err error) {
	reloaded, err := NewFileStorage(fs.path, fs.region, fs.coldBootMS)
	if err != nil {
		return 0, 0, err
	}
	return len(reloaded.state.Branches), len(reloaded.state.Instances), nil
}
