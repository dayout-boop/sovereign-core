package main

import (
	"fmt"
	"strings"
	"sync"
)

// ─────────────────────────────────────────────────────────────────────────
// 라우팅 레이어 (connection_uri + SNI v0.1).
// connection_uri 가 발급된 뒤, 클라이언트 연결이 어느 백엔드로 가는지 푼다.
//   - RoutingRegistry: endpoint_id -> 백엔드 + 상태. 컨트롤플레인이 채움.
//   - resolveEndpointID: SNI → options → password 폴백 체인.
//   - Resolve: suspended 면 WakeHook 으로 깨움(scale-to-zero 데이터플레인 측).
// 진짜 프록시는 crypto/tls GetConfigForClient 로 SNI 추출 후 이 로직 사용.
// ─────────────────────────────────────────────────────────────────────────

type backend struct {
	addr  string // 컴퓨트 도달점 (host:port)
	state string // active|suspended
}

// WakeHook = scale-to-zero 깨우기. 진짜는 StoragePort.StartEndpoint(웜풀/PVM).
// 반환 bootMS = hold 시간 = 즉시성 갭의 데이터플레인 측 실측치.
type WakeHook func(endpointID string) (addr string, bootMS int64, err error)

type RoutingRegistry struct {
	mu   sync.RWMutex
	tbl  map[string]*backend
	wake WakeHook
}

func NewRoutingRegistry(wake WakeHook) *RoutingRegistry {
	return &RoutingRegistry{tbl: map[string]*backend{}, wake: wake}
}

func (r *RoutingRegistry) Register(endpointID, addr, state string) {
	r.mu.Lock(); defer r.mu.Unlock()
	r.tbl[endpointID] = &backend{addr: addr, state: state}
}
func (r *RoutingRegistry) SetState(endpointID, state string) {
	r.mu.Lock(); defer r.mu.Unlock()
	if b, ok := r.tbl[endpointID]; ok { b.state = state }
}
func (r *RoutingRegistry) Deregister(endpointID string) {
	r.mu.Lock(); defer r.mu.Unlock()
	delete(r.tbl, endpointID)
}

// Resolve = 라우팅 흐름 ②③. suspended 면 깨워서 active 백엔드 반환.
func (r *RoutingRegistry) Resolve(endpointID string) (addr string, bootMS int64, err error) {
	r.mu.RLock()
	b, ok := r.tbl[endpointID]
	r.mu.RUnlock()
	if !ok {
		return "", 0, fmt.Errorf("endpoint %s not found", endpointID) // 거부
	}
	if b.state == "active" {
		return b.addr, 0, nil // 웜: 즉시
	}
	// suspended → WakeHook (즉시성 갭이 여기. 웜풀 히트면 ~0)
	newAddr, ms, werr := r.wake(endpointID)
	if werr != nil {
		return "", ms, fmt.Errorf("wake %s: %w", endpointID, werr)
	}
	r.mu.Lock()
	b.addr = newAddr
	b.state = "active"
	r.mu.Unlock()
	return newAddr, ms, nil
}

// ─────────────────────────────────────────────────────────────────────────
// endpoint_id 식별 — 3중 폴백 (SNI → options → password 접두).
// 모든 표준 Postgres 클라이언트가 최소 하나로 도달하게.
// ─────────────────────────────────────────────────────────────────────────

type ConnAttempt struct {
	SNI      string // TLS ClientHello.ServerName  (예: "ep_szF.ap-northeast-2.internal")
	Options  string // libpq options             (예: "endpoint=ep_szF")
	Password string // 자격증명                   (예: "endpoint=ep_szF;realpw")
}

func resolveEndpointID(a ConnAttempt) (endpointID, password string, method string, err error) {
	// 1) SNI: 첫 라벨. "-pooler" 접미는 떼기.
	if a.SNI != "" {
		label := a.SNI
		if i := strings.IndexByte(label, '.'); i >= 0 {
			label = label[:i]
		}
		label = strings.TrimSuffix(label, "-pooler")
		if label != "" {
			return label, a.Password, "sni", nil
		}
	}
	// 2) libpq options: "endpoint=<id>"
	if id := parseKV(a.Options, "endpoint"); id != "" {
		return id, a.Password, "options", nil
	}
	// 3) password 접두: "endpoint=<id>;<realpw>"
	if strings.HasPrefix(a.Password, "endpoint=") {
		rest := strings.TrimPrefix(a.Password, "endpoint=")
		if i := strings.IndexByte(rest, ';'); i >= 0 {
			return rest[:i], rest[i+1:], "password", nil
		}
	}
	return "", "", "", fmt.Errorf("no endpoint id (SNI/options/password 모두 없음)")
}

// "k=v k2=v2" 또는 "k=v,k2=v2" 에서 key 추출(간이).
func parseKV(s, key string) string {
	for _, tok := range strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == ',' }) {
		if kv := strings.SplitN(tok, "=", 2); len(kv) == 2 && kv[0] == key {
			return kv[1]
		}
	}
	return ""
}
