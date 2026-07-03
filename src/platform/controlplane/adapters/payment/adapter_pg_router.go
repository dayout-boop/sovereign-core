package main

import (
	"context"
	"fmt"
	"sync"
)

// ─────────────────────────────────────────────────────────────────────────
// adapter_pg_router.go — PGRouterPort 구현체
//
// 설계 원칙:
//   - 비즈니스 로직은 PGRouterPort만 바라본다.
//   - RegisterPG로 신규 PG를 등록하면 기존 코드 변경 없이 확장.
//   - 기본 라우팅 규칙: KR → Toss, EU → StripeEU, 글로벌 → StripeUS.
//   - 테넌트별 오버라이드 지원 (엔터프라이즈 고객 전용 PG 지정).
// ─────────────────────────────────────────────────────────────────────────

// PGRouter — PGRouterPort 구현체.
type PGRouter struct {
	mu              sync.RWMutex
	regionRoutes    map[string]PGKind // region → PGKind
	tenantOverrides map[string]PGKind // orgID → PGKind (엔터프라이즈 전용)
	store           *Store
}

// 컴파일 타임 인터페이스 계약 검증.
var _ PGRouterPort = (*PGRouter)(nil)

// NewPGRouter — 기본 라우팅 규칙이 적용된 PGRouter 생성.
func NewPGRouter(store *Store) *PGRouter {
	r := &PGRouter{
		regionRoutes: map[string]PGKind{
			// 국내 리전: Toss Payments
			"ap-northeast-2": PGToss,  // 서울
			"kr-central-1":   PGToss,  // 국내 추가 리전
			// EU 리전: Stripe EU (데이터 레지던시 준수)
			"eu-west-1":      PGStripe,
			"eu-central-1":   PGStripe,
			"eu-north-1":     PGStripe,
			// 글로벌/기타: Stripe US
			"us-east-1":      PGStripe,
			"us-west-2":      PGStripe,
			"ap-southeast-1": PGStripe, // 싱가포르
			"ap-northeast-1": PGStripe, // 도쿄
		},
		tenantOverrides: make(map[string]PGKind),
		store:           store,
	}
	return r
}

// RouteByTenant — 테넌트 ID 기준 PG 선택.
// 우선순위: 테넌트 오버라이드 > 테넌트 리전 > 기본 규칙.
func (r *PGRouter) RouteByTenant(ctx context.Context, orgID string) (PGKind, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// 1. 테넌트 전용 오버라이드 확인 (엔터프라이즈 고객).
	if kind, ok := r.tenantOverrides[orgID]; ok {
		return kind, nil
	}

	// 2. 테넌트 리전 기반 라우팅.
	orgRegion, found := r.store.getOrgRegion(orgID)
	if !found {
		return PGMock, fmt.Errorf("org not found: %s", orgID)
	}
	if kind, ok := r.regionRoutes[orgRegion]; ok {
		return kind, nil
	}

	// 3. 기본 폴백: Stripe (글로벌).
	return PGStripe, nil
}

// RouteByRegion — 리전 코드 기준 PG 선택.
func (r *PGRouter) RouteByRegion(ctx context.Context, region string) (PGKind, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if kind, ok := r.regionRoutes[region]; ok {
		return kind, nil
	}
	// 알 수 없는 리전: 글로벌 Stripe 폴백.
	return PGStripe, nil
}

// RegisterPG — 신규 리전-PG 매핑 등록 (런타임 확장).
// 신규 리전 진출 시 이 메서드 한 줄만 추가하면 된다.
func (r *PGRouter) RegisterPG(region string, kind PGKind) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.regionRoutes[region] = kind
}

// SetTenantOverride — 특정 테넌트에 전용 PG 지정 (엔터프라이즈).
func (r *PGRouter) SetTenantOverride(orgID string, kind PGKind) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tenantOverrides[orgID] = kind
}

// RemoveTenantOverride — 테넌트 전용 PG 오버라이드 제거.
func (r *PGRouter) RemoveTenantOverride(orgID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tenantOverrides, orgID)
}
