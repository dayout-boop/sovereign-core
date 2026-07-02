package main

import "context"

// ─────────────────────────────────────────────────────────────────────────
// 포트 (D13 포트-어댑터) — 공급자 비종속 경계.
// mock → 진짜(자체 엔진 / AWS KMS / Vault / Stripe)로 "국소 교체".
// ─────────────────────────────────────────────────────────────────────────

// StoragePort — 자체호스팅 엔진 경계.
// 웜풀 도입으로 "컴퓨트 콜드부팅"(BootInstance)과 "브랜치 부착"(AttachBranch)을 분리.
// 웜풀은 BootInstance 를 미리 호출해 컴퓨트를 데워두고, 요청 시 AttachBranch 만.
type StoragePort interface {
	CreateBranch(ctx context.Context, orgID, projectID, parentBranchID string) (branchID string, err error)
	DeleteBranch(ctx context.Context, orgID, branchID string) error

	// BootInstance = 비어있는 컴퓨트 콜드부팅(비싼 작업). 진짜: PVM/Firecracker 부팅.
	// 반환 bootMS = 콜드부팅 실측치. 웜풀이 미리 호출해두면 요청 경로에서 0.
	BootInstance(ctx context.Context, region string) (instanceID, host string, bootMS int64, err error)
	// AttachBranch = 데워둔 컴퓨트에 브랜치 데이터 부착(싼 작업).
	AttachBranch(ctx context.Context, instanceID, branchID string) error

	SuspendEndpoint(ctx context.Context, orgID, endpointID string) error // scale-to-zero
	DeleteEndpoint(ctx context.Context, orgID, endpointID string) error
}

// AuthPort — 양방향 OIDC/SAML. SP(소비)+IdP(발급).
type AuthPort interface {
	IssueToken(ctx context.Context, orgID, userID, role string) (jwt string, err error)
	VerifyToken(ctx context.Context, jwt string) (claims Claims, err error)
	JWKS(ctx context.Context) (jwksJSON string, err error)
}

type Claims struct {
	OrgID  string // 안전 클레임에만(위변조 불가 경계)
	UserID string
	Role   string
}

// KmsPort — 봉투암호화. 평문 미보유. 테넌트별 KEK 프로비전.
type KmsPort interface {
	ProvisionTenantKEK(ctx context.Context, orgID string) (kmsKeyID string, err error)
}

// SecretPort — DB 자격증명 등.
type SecretPort interface {
	IssueDBCredential(ctx context.Context, orgID, endpointID string) (user, password string, err error)
}

// PaymentPort — 2 파이프라인(platform / tenant-resell). MVP는 platform만.
type PaymentPort interface {
	CreateCustomer(ctx context.Context, orgID string) (billingID string, err error)
}
