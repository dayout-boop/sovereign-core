package main

import (
	"context"
	"fmt"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────
// App = 포트 묶음 + 웜풀 + 라우팅 + P0 루프 로직.
// 컴퓨트 기동은 웜풀 Acquire(히트~0/미스=콜드)로 통일. 비동기 완료는 잠금 하.
// ─────────────────────────────────────────────────────────────────────────

type App struct {
	store    *Store
	storage  StoragePort
	auth     AuthPort
	kms      KmsPort
	secret   SecretPort
	payment  CustomerPort // L0 고객 등록. 구독/인보이스/환불은 별도 포트 어댑터.
	warmpool *WarmPool
	registry *RoutingRegistry
	ledger   *Ledger
	region   string

	// 결제 확장 포트
	pgRouter  PGRouterPort
	operator  OperatorPort
	consent   ConsentPort

	// 알림 포트
	notif     NotificationPort

	// 인증 확장 포트
	pkce      OAuth2PKCEPort
	device    DeviceFlowPort
	apiKey    APIKeyPort

	// HA 포트
	heartbeat HeartbeatPort
	failover  FailoverPort
	backup    BackupPort

	// 자동화 봇
	retryBot      *PaymentRetryBot
	slaBot        *SLACompensationBot
	settleBot     *SettlementBot
	failoverBot   *FailoverBot
}

// 기본 정책 = 부하 비례(추천). 갈아끼우려면 NewAppWithPolicy.
func NewApp() *App {
	return NewAppWithPolicy(LoadProportionalPolicy{Buffer: 1.5, Min: 2, Max: 64})
}

func NewAppWithPolicy(policy WarmPoolPolicy) *App {
	region := "ap-northeast-2"
	a := &App{
		store:   NewStore(),
		storage: &MockStorage{region: region, coldBootMS: 150}, // 콜드부팅 시뮬 150ms
		auth:    NewMockAuth(), // [sovereign_core] HMAC-SHA256 서명 기반 (T9-A 위조 토큰 결함 수정)
		kms:     &MockKms{},
		secret:  &MockSecret{},
		payment: &MockPayment{},
		ledger:  NewLedger(),
		region:  region,
	}
	a.warmpool = NewWarmPool(a.storage, region, policy)

	// 이벤트 버스 초기화 (인메모리).
	bus := NewInMemoryEventBus()

	// 결제 확장 포트 초기화.
	a.pgRouter = NewPGRouter(a.store)
	a.notif = NewNotificationAdapter(a.store, bus)
	a.operator = NewOperatorAdapter(bus)
	a.consent = NewConsentAdapter(bus, a.notif)

	// 인증 확장 포트 초기화.
	a.pkce = NewOAuth2PKCEAdapter()
	a.device = NewDeviceFlowAdapter()
	a.apiKey = NewAPIKeyAdapter()

	// HA 포트 초기화.
	a.heartbeat = NewHeartbeatAdapter(bus)
	a.failover = NewFailoverAdapter(bus)
	a.backup = NewBackupAdapter(bus)

	// 자동화 봇 초기화.
	var paymentFailure PaymentFailurePort
	if mp, ok := a.payment.(*MockPayment); ok {
		paymentFailure = mp
	}
	a.retryBot = NewPaymentRetryBot(paymentFailure, a.notif, bus)
	a.slaBot = NewSLACompensationBot(a.notif, bus)
	a.settleBot = NewSettlementBot(&MockInvoice{}, a.notif, bus, a.store)
	a.failoverBot = NewFailoverBot(a.heartbeat, a.failover, a.backup, a.notif, bus)

	// WakeHook = scale-to-zero 깨우기 → 웜풀에서 확보 + 브랜치 부착.
	a.registry = NewRoutingRegistry(func(endpointID string) (string, int64, error) {
		orgID, branchID, _, _, ok := a.store.endpointRef(endpointID)
		if !ok {
			return "", 0, fmt.Errorf("endpoint gone")
		}
		inst, _, bootMS, err := a.warmpool.Acquire(context.Background())
		if err != nil {
			return "", bootMS, err
		}
		if err := a.storage.AttachBranch(context.Background(), inst.ID, branchID); err != nil {
			return "", bootMS, err
		}
		_ = a.store.transitionEndpoint(orgID, endpointID, EndpointActive)
		return inst.Host, bootMS, nil
	})
	return a
}

// ── 원자적 온보딩: org + KEK + billing + owner 한 번에. 부분생성 금지. ──
func (a *App) Signup(ctx context.Context, name, ownerUserID string) (*Org, string, error) {
	kekID, err := a.kms.ProvisionTenantKEK(ctx, "")
	if err != nil {
		return nil, "", fmt.Errorf("kek: %w", err)
	}
	org := &Org{
		ID: newID("org"), Name: name, Region: a.region,
		KMSKeyID: kekID, Plan: "free", CreatedAt: time.Now(),
	}
	billingID, err := a.payment.CreateCustomer(ctx, org.ID)
	if err != nil {
		return nil, "", fmt.Errorf("billing: %w", err)
	}
	org.BillingID = billingID
	// 토큰을 먼저 발급받고, 성공한 뒤에만 store 에 커밋한다.
	// (이전엔 store.put/addMembership 후 IssueToken 실패 시 좀비 org/membership 잔존 = T5 결함)
	token, err := a.auth.IssueToken(ctx, org.ID, ownerUserID, "owner")
	if err != nil {
		return nil, "", fmt.Errorf("token: %w", err)
	}
	a.store.put(org)
	a.store.addMembership(Membership{OrgID: org.ID, UserID: ownerUserID, Role: "owner"})
	return org, token, nil
}

// ── 프로젝트 생성 (async) + root branch. ──
func (a *App) CreateProject(ctx context.Context, orgID, name string) *Operation {
	op := &Operation{ID: newID("op"), OrgID: orgID, Kind: "create_project", Status: "running", Created: time.Now()}
	a.store.put(op)
	go func() {
		brID, err := a.storage.CreateBranch(ctx, orgID, "", "")
		if err != nil {
			a.store.failOp(op.ID, err.Error())
			return
		}
		proj := &Project{ID: newID("proj"), OrgID: orgID, Name: name, Region: a.region, RootBranch: brID, CreatedAt: time.Now()}
		a.store.put(proj)
		a.store.put(&Branch{ID: brID, OrgID: orgID, ProjectID: proj.ID, State: BranchReady, CreatedAt: time.Now()})
		a.store.addMetering(MeteringEvent{OrgID: orgID, Kind: "branch_ops", Value: 1, At: time.Now()})
		a.store.completeOp(op.ID, map[string]string{"project_id": proj.ID, "root_branch_id": brID})
	}()
	return op
}

// ── 브랜치 생성 (CoW, async). ──
func (a *App) CreateBranch(ctx context.Context, orgID, projectID, parentBranchID string) *Operation {
	op := &Operation{ID: newID("op"), OrgID: orgID, Kind: "create_branch", Status: "running", Created: time.Now()}
	a.store.put(op)
	go func() {
		if _, ok := a.store.getProject(orgID, projectID); !ok {
			a.store.failOp(op.ID, "project not found")
			return
		}
		brID, err := a.storage.CreateBranch(ctx, orgID, projectID, parentBranchID)
		if err != nil {
			a.store.failOp(op.ID, err.Error())
			return
		}
		a.store.put(&Branch{ID: brID, OrgID: orgID, ProjectID: projectID, ParentID: parentBranchID, State: BranchCreating, CreatedAt: time.Now()})
		_ = a.store.transitionBranch(orgID, brID, BranchReady)
		a.store.addMetering(MeteringEvent{OrgID: orgID, Kind: "branch_ops", Value: 1, At: time.Now()})
		a.store.completeOp(op.ID, map[string]string{"branch_id": brID})
	}()
	return op
}

// ── 엔드포인트 기동 (async) → 웜풀 Acquire + AttachBranch → connection_uri. ──
func (a *App) StartEndpoint(ctx context.Context, orgID, branchID string, minCU, maxCU float64) *Operation {
	op := &Operation{ID: newID("op"), OrgID: orgID, Kind: "start_endpoint", Status: "running", Created: time.Now()}
	a.store.put(op)
	go func() {
		if _, ok := a.store.getBranch(orgID, branchID); !ok {
			a.store.failOp(op.ID, "branch not found")
			return
		}
		// 웜풀에서 컴퓨트 확보. 히트=boot 0, 미스=콜드부팅 실측. (즉시성 갭)
		inst, hit, bootMS, err := a.warmpool.Acquire(ctx)
		if err != nil {
			a.store.failOp(op.ID, err.Error())
			return
		}
		if err := a.storage.AttachBranch(ctx, inst.ID, branchID); err != nil {
			a.store.failOp(op.ID, err.Error())
			return
		}
		epID := newID("ep")
		logical := func(suffix string) string { return fmt.Sprintf("%s%s.%s.internal", epID, suffix, a.region) }
		user, pw, err := a.secret.IssueDBCredential(ctx, orgID, epID)
		if err != nil {
			a.store.failOp(op.ID, err.Error())
			return
		}
		// connection_uri host = endpoint_id(논리, SNI 키). 실제 백엔드 = inst.Host(물리).
		uri := fmt.Sprintf("postgresql://%s:%s@%s:5432/main?sslmode=require", user, pw, logical(""))
		pooled := fmt.Sprintf("postgresql://%s:%s@%s:5432/main?sslmode=require", user, pw, logical("-pooler"))

		ep := &Endpoint{
			ID: epID, OrgID: orgID, BranchID: branchID, State: EndpointCreating,
			AutoscaleMin: minCU, AutoscaleMax: maxCU, SuspendAfterS: 300,
			ConnectionURI: uri, CreatedAt: time.Now(),
		}
		a.store.put(ep)
		_ = a.store.transitionEndpoint(orgID, epID, EndpointActive)
		a.registry.Register(epID, inst.Host, "active") // endpoint_id → 물리 백엔드
		a.store.addMetering(MeteringEvent{OrgID: orgID, Kind: "cu_hours", Value: 0, At: time.Now()})
		a.store.completeOp(op.ID, map[string]any{
			"endpoint_id":       epID,
			"connection_uri":    uri,
			"connection_pooled": pooled,
			"boot_ms":           bootMS,
			"warm_hit":          hit,
		})
	}()
	return op
}

func (a *App) SuspendEndpoint(ctx context.Context, orgID, epID string) error {
	if _, ok := a.store.getEndpoint(orgID, epID); !ok {
		return fmt.Errorf("endpoint not found")
	}
	if err := a.storage.SuspendEndpoint(ctx, orgID, epID); err != nil {
		return err
	}
	a.registry.SetState(epID, "suspended") // 다음 연결 시 WakeHook
	return a.store.transitionEndpoint(orgID, epID, EndpointSuspended)
}

// ResolveConnection = 데이터플레인 입구. SNI/options/password → 백엔드.
func (a *App) ResolveConnection(att ConnAttempt) (endpointID, backendAddr string, bootMS int64, err error) {
	epID, _, _, err := resolveEndpointID(att)
	if err != nil {
		return "", "", 0, err
	}
	addr, ms, err := a.registry.Resolve(epID)
	if err != nil {
		return epID, "", ms, err
	}
	return epID, addr, ms, nil
}

func (a *App) Usage(orgID string) map[string]float64 { return a.store.usageRollup(orgID) }

// waitIdemp — 멱등 예약(sentinel)가 확정 op.ID 로 바뀔 때까지 짧게 대기.
// 동시 요청 중 선점 실패자가 확정 id 를 받아 replay 하기 위함. 미확정 시 "".
func (a *App) waitIdemp(key string, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if id, ok := a.store.idempResolve(key); ok {
			return id
		}
		time.Sleep(time.Millisecond)
	}
	return ""
}

// 폴링 헬퍼: op 완료까지 대기. 값 복사 반환(레이스 없음).
func (a *App) waitOp(orgID, opID string, timeout time.Duration) (Operation, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if op, ok := a.store.opSnapshot(orgID, opID); ok && op.Status != "running" {
			return op, nil
		}
		time.Sleep(time.Millisecond)
	}
	return Operation{}, fmt.Errorf("operation %s timeout", opID)
}
