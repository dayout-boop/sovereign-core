package main

import (
	"fmt"
	"sync"
)

// ─────────────────────────────────────────────────────────────────────────
// 인메모리 metaDB (mock). 진짜는 Postgres+RLS.
// RLS 는 "모든 조회 org_id 필터"로 시뮬. 비동기 op 완료는 잠금 하에서만(레이스 방지).
// ─────────────────────────────────────────────────────────────────────────

type Store struct {
	mu          sync.RWMutex
	orgs        map[string]*Org
	memberships []Membership
	projects    map[string]*Project
	branches    map[string]*Branch
	endpoints   map[string]*Endpoint
	operations  map[string]*Operation
	metering    []MeteringEvent
	idemp       map[string]string
}

func NewStore() *Store {
	return &Store{
		orgs: map[string]*Org{}, projects: map[string]*Project{},
		branches: map[string]*Branch{}, endpoints: map[string]*Endpoint{},
		operations: map[string]*Operation{}, idemp: map[string]string{},
	}
}

// RLS 경계: orgID 불일치 시 "없음"으로 취급.
func (s *Store) getProject(orgID, id string) (*Project, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.projects[id]
	if !ok || p.OrgID != orgID {
		return nil, false
	}
	return p, true
}
func (s *Store) getBranch(orgID, id string) (*Branch, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.branches[id]
	if !ok || b.OrgID != orgID {
		return nil, false
	}
	return b, true
}
func (s *Store) getEndpoint(orgID, id string) (*Endpoint, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.endpoints[id]
	if !ok || e.OrgID != orgID {
		return nil, false
	}
	return e, true
}

// endpointRef — WakeHook 용(org 모름). 잠금 하에서 필요 필드만 복사.
func (s *Store) endpointRef(id string) (orgID, branchID string, minCU, maxCU float64, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, found := s.endpoints[id]
	if !found {
		return "", "", 0, 0, false
	}
	return e.OrgID, e.BranchID, e.AutoscaleMin, e.AutoscaleMax, true
}

func (s *Store) put(v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch t := v.(type) {
	case *Org:
		s.orgs[t.ID] = t
	case *Project:
		s.projects[t.ID] = t
	case *Branch:
		s.branches[t.ID] = t
	case *Endpoint:
		s.endpoints[t.ID] = t
	case *Operation:
		s.operations[t.ID] = t
	}
}

func (s *Store) addMembership(m Membership) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.memberships = append(s.memberships, m)
}

func (s *Store) addMetering(e MeteringEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metering = append(s.metering, e)
}

func (s *Store) usageRollup(orgID string) map[string]float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]float64{}
	for _, e := range s.metering {
		if e.OrgID == orgID {
			out[e.Kind] += e.Value
		}
	}
	return out
}

// 멱등성.
func (s *Store) idempLookup(key string) (string, bool) {
	if key == "" {
		return "", false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.idemp[key]
	return id, ok
}
func (s *Store) idempStore(key, id string) {
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.idemp[key] = id
}

// ── 비동기 op 완료 — 잠금 하에서만. (이전 레이스의 해소 지점) ──
func (s *Store) completeOp(id string, result any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.operations[id]; ok {
		op.Status = "succeeded"
		op.Result = result
	}
}
func (s *Store) failOp(id, errStr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.operations[id]; ok {
		op.Status = "failed"
		op.Error = errStr
	}
}

// opSnapshot — 값 복사 반환(공유 포인터 미노출 → 읽기 레이스 없음).
// Result 맵은 completeOp 에서 1회 기록 후 불변이라 복사 후 읽기 안전.
func (s *Store) opSnapshot(orgID, id string) (Operation, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	op, ok := s.operations[id]
	if !ok || op.OrgID != orgID {
		return Operation{}, false
	}
	return *op, true
}

// 상태 전이(검증 포함).
func (s *Store) transitionBranch(orgID, id, to string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.branches[id]
	if !ok || b.OrgID != orgID {
		return fmt.Errorf("branch not found")
	}
	if err := canTransition(branchTransitions, b.State, to); err != nil {
		return err
	}
	b.State = to
	return nil
}
func (s *Store) transitionEndpoint(orgID, id, to string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.endpoints[id]
	if !ok || e.OrgID != orgID {
		return fmt.Errorf("endpoint not found")
	}
	if err := canTransition(endpointTransitions, e.State, to); err != nil {
		return err
	}
	e.State = to
	return nil
}
