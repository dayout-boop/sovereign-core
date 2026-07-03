# KNOWN_ISSUES.md — 현재 코드 동결 선언

> **동결 선언일**: 2026-07-03
> **동결 이유**: 포트(인터페이스) 설계 기반 오류. 인프라 프로비저닝 후 L3 실제 구현 시점에 전면 교체.
> **현재 코드 상태**: 인메모리 임시 구현(L2 Stub 미완). 프로덕션 배포 불가.

---

## 문서 현황 — 실제 코드 설계서 vs 보고서 분류

현재까지 생성된 MD 파일은 총 **57개**입니다.
이 중 코드를 짜기 전에 작성된 실제 코드 설계서는 **0개**입니다.
모든 문서는 코드 작성 후 사후 보고서 또는 조사 결과물로 작성되었습니다.

| 분류 | 파일 수 | 해당 파일 |
|------|---------|-----------|
| **조사/리서치 보고서** | 14 | PAYMENT_RESEARCH.md, RESEARCH_4SERVICES.md, RESEARCH_VULTR.md, WORKLOAD_RESEARCH.md, LATEST_TECH_RESEARCH.md, RESEARCH_LICENSES.md, RESEARCH_REFS.md, PRICING_DATA.md, ENGINE_CURRENT_LIMITS.md, ENGINE_VERIFICATION.md, NEON_ENGINE_PRODUCTION_LIMITS.md, LATENCY_CONSTANTS.md, LATENCY_MODIFICATION_BENEFIT.md, PAYMENT_AUTH_RESEARCH_V2.md |
| **사후 검증 보고서** | 10 | CODE_AUDIT_RESULT.md, CODE_GAP_AUDIT.md, FINAL_REPORT_ABC.md, FINAL_REPORT_LEDGER_BILLING.md, FINAL_VERIFICATION_REPORT.md, VERIFICATION_REPORT.md, NEON_ENGINE_PATCH_REPORT.md, STAGE2_REPORT.md, STAGE3_SECURITY_REPORT.md, NEON_4_CORE_PATCH_REPORT.md |
| **설계 결정 문서 (코드 후 작성)** | 12 | PAYMENT_AUTH_DESIGN_DECISIONS.md, HA_BACKUP_RECOVERY_DESIGN.md, INFRA_DESIGN_DECISIONS.md, MASTER_ARCHITECTURE_V1.md, SOVEREIGN_INFRA_DESIGN.md, DESIGN_BEDROCK_VULTR.md, DESIGN_MODAL_VULTR.md, DESIGN_NEON_VULTR.md, DESIGN_OPENROUTER_VULTR.md, NEON_CODE_LEVEL_DESIGN.md, NEON_AI_MODIFICATION_PLAN.md, ENGINE_MODIFICATION_PROPOSAL.md |
| **진행 상태 스냅샷** | 8 | PHASE1_PROGRESS.md, PROGRESS_SNAPSHOT.md, WORK_STATE.md, VALIDATION_ROADMAP.md, DUAL_TRACK_ROADMAP.md, ROADMAP_SOVEREIGN_INFRA.md, REDIAGNOSIS.md, INTEGRATED_PLANNING_AND_ANSWERS.md |
| **아키텍처 분석** | 8 | CONTROL_VS_DATA_PLANE.md, AUTOSPLIT_DEEP.md, HA_DEEP.md, LIVEMIGRATION_DEEP.md, SHARDING_DEEP.md, ENGINE_DIFFERENTIATION_PROPOSAL.md, WHY_MIGRATION_AND_IDLE.md, SLO_TABLE.md |
| **비즈니스/운영** | 5 | BUSINESS_PLAN_V2.md, BUSINESS_REDESIGN.md, OPERATIONS_AND_GLOBAL_DESIGN.md, PAYMENT_POLICY_DESIGN.md, TCO_PERFORMANCE_REPORT.md |

**핵심 문제**: 코드 설계서(포트 정의, 도메인 언어, 에러 타입, 구현 계층 명세)가 단 한 개도 없습니다.
모든 문서가 코드를 짠 후 작성되었습니다.

---

## 알려진 코드 문제 전수 목록

### 1. 포트 설계 오류 — 기술 구현 방식 노출

| 파일 | 라인 | 문제 | 올바른 방향 |
|------|------|------|-------------|
| `ha_ports.go` | 36 | `WALLag int64` — WAL은 Neon/PostgreSQL 전용 개념 | `SyncLagBytes int64` 또는 `SyncStatus string` |
| `ha_ports.go` | 37 | `PhiScore float64` — Phi-accrual 알고리즘 내부 값 | `FailureScore float64` 또는 `IsHealthy bool` |
| `ha_ports.go` | 55 | `WatchFailure(handler func(nodeID string, phi float64))` — 콜백에 알고리즘 값 노출 | `func(nodeID string, reason FailoverReason)` |
| `ha_ports.go` | 104~107 | `BackupTypeWAL`, `BackupTypeEtcd` — 특정 기술 스택 강제 | `BackupTypeContinuous`, `BackupTypeFullSnapshot` |
| `ha_ports.go` | 116 | `WALPosition string` — PostgreSQL LSN 내부 개념 | `RecoveryPoint string` |
| `auth_ports.go` | 22~26 | `PKCEChallenge` 구조체 — OAuth2 프로토콜 세부사항 포트 노출 | 포트에서 제거, 어댑터 내부로 이동 |

### 2. `return nil` 한 줄짜리 미구현 메서드

| 파일 | 메서드 | 실제 필요한 동작 | 인프라 전제조건 |
|------|--------|-----------------|----------------|
| `adapter_ha.go:225` | `PromoteStandby` | Neon Compute 시작 + Safekeeper 라우팅 전환 | Neon 엔진 배포 |
| `adapter_ha.go:218` | `FenceNode` | STONITH/IPMI 실제 전원 차단 | IPMI 장치 또는 Fence Agent |
| `adapter_ha.go:230` | `SwitchVIP` | BGP/Keepalived VIP 전환 | 네트워크 인프라 |
| `adapter_ha.go:332` | `RestoreFromBackup` | Velero 복원 명령 실행 | Velero + MinIO 배포 |

### 3. 포트 추상화를 뚫는 구체 타입 캐스팅

```go
// bots.go:268 — 포트 계약 파괴
if ha, ok := b.heartbeat.(*HeartbeatAdapter); ok {
    ha.CheckAndNotifyFailures(ctx)
}
```

`HeartbeatPort`에 `TriggerHealthCheck(ctx) error` 메서드가 없어서 봇이 구체 타입으로 직접 접근합니다.

### 4. 하드코딩된 값

```go
// adapter_ha.go:196
NewActiveID: "node-b", // 실제: Witness 투표 결과로 결정
```

Witness 투표 결과를 받아야 할 자리에 문자열 리터럴이 있습니다.

### 5. 테스트가 어댑터 내부 구조체를 직접 조작

`auth_audit_test.go`, `ha_audit_test.go`에서 포트 인터페이스를 통하지 않고 어댑터 내부 맵(`sessions`, `phiMap`)을 직접 잠그고 조작합니다. 이것은 포트 계약이 테스트 가능한 수준으로 설계되지 않았음을 의미합니다.

### 6. L2 Stub 미완성 — `return nil` vs `ErrNotImplemented`

현재 Stub들이 `return nil`을 반환하여 "성공"처럼 동작합니다. 실제로는 아무것도 하지 않으면서 성공을 반환하므로 상위 코드가 정상 동작한다고 착각합니다.

---

## 동결 조건 해제 기준

다음 조건이 모두 충족될 때 동결을 해제하고 L3 실제 구현으로 교체합니다.

| 조건 | 상태 |
|------|------|
| 물리 서버 2대 + Witness 서버 1대 프로비저닝 | 미완 |
| 서버 간 전용 네트워크 구성 | 미완 |
| Neon 엔진 배포 및 Safekeeper 구성 | 미완 |
| MinIO + Velero 배포 | 미완 |
| IPMI/Fence Agent 구성 | 미완 |
| BGP/Keepalived VIP 구성 | 미완 |
| 코드 설계서(DESIGN_SPEC_*.md) 작성 완료 | 미완 |
