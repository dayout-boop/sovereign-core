# Sovereign Core — 디렉토리 재구조화 계획

## 설계 원칙
- **레이어 축**: 플랫폼이 어떤 계층에서 동작하는지 (control / data / engine)
- **엔진 축**: 어떤 엔진/런타임을 다루는지 (neon, compute, storage, gateway)
- **기능 축**: 무엇을 하는지 (payment, auth, ha, notification, billing)
- **제품 축**: 어떤 제품 파이프라인인지 (governance, training, ops)
- 나중에 신규 엔진(e.g. custom-db), 신규 제품(e.g. analytics), 신규 리전 어댑터가 들어와도 기존 구조 변경 없이 폴더 추가만으로 확장 가능

## 목표 구조

```
sovereign-core/
├── src/
│   │
│   ├── platform/                        ← [레이어] 플랫폼 공통 기반
│   │   ├── controlplane/                ← 컨트롤 플레인 (오케스트레이션)
│   │   │   ├── domain/                  ← 도메인 모델 / 스토어 / 원장
│   │   │   │   ├── model.go
│   │   │   │   ├── store.go
│   │   │   │   └── ledger.go
│   │   │   ├── ports/                   ← 포트(인터페이스) — 기능별 분리
│   │   │   │   ├── payment.go           (ports.go 중 결제 관련)
│   │   │   │   ├── auth.go              (auth_ports.go)
│   │   │   │   ├── ha.go                (ha_ports.go)
│   │   │   │   └── infra.go             (infra_ports.go)
│   │   │   ├── adapters/                ← 어댑터 — 기능별 분리
│   │   │   │   ├── payment/
│   │   │   │   │   ├── pg_router.go     (adapter_pg_router.go)
│   │   │   │   │   ├── billing.go       (adapter_billing.go)
│   │   │   │   │   └── payment_full.go  (adapter_payment_full.go)
│   │   │   │   ├── auth/
│   │   │   │   │   └── extended.go      (adapter_auth_extended.go)
│   │   │   │   ├── notification/
│   │   │   │   │   └── notification.go  (adapter_notification.go)
│   │   │   │   ├── operator/
│   │   │   │   │   ├── operator.go      (adapter_operator.go)
│   │   │   │   │   └── consent.go       (adapter_consent.go)
│   │   │   │   ├── ha/
│   │   │   │   │   └── ha.go            (adapter_ha.go)
│   │   │   │   ├── storage/
│   │   │   │   │   └── filestorage.go   (adapter_filestorage.go)
│   │   │   │   └── infra/
│   │   │   │       └── infra.go         (adapter_infra.go)
│   │   │   ├── bots/                    ← 자동화 봇
│   │   │   │   └── bots.go
│   │   │   ├── app/                     ← 앱 조립 / 라우팅 / 핸들러
│   │   │   │   ├── app.go
│   │   │   │   ├── handlers.go
│   │   │   │   ├── routing.go
│   │   │   │   └── warmpool.go
│   │   │   └── main.go
│   │   │
│   │   └── dataplane/                   ← 데이터 플레인 (향후 확장)
│   │       └── .gitkeep
│   │
│   ├── engines/                         ← [엔진 축] OSS 엔진 패치/확장
│   │   ├── neon/                        ← Neon DB 엔진 (Rust)
│   │   │   ├── heartbeater/             (patches/heartbeater → 이동)
│   │   │   │   ├── heartbeater.rs
│   │   │   │   ├── phi_accrual_detector.rs
│   │   │   │   ├── lib.rs
│   │   │   │   └── *.patch
│   │   │   ├── reconciler/              (patches/reconciler → 이동)
│   │   │   │   ├── reconciler.rs
│   │   │   │   └── *.patch
│   │   │   └── sovereign_core_all.patch (patches_v2 통합 패치)
│   │   │
│   │   └── compute/                     ← 향후: 커스텀 컴퓨트 엔진
│   │       └── .gitkeep
│   │
│   ├── products/                        ← [제품 축] 제품별 파이프라인
│   │   ├── governance/                  (governance → 이동)
│   │   │   ├── change_pipeline/
│   │   │   └── engine_boundary_sim/
│   │   └── training/                    (training → 이동)
│   │       └── dev_engine_flow/
│   │
│   └── ops/                             ← [운영 축] 향후: 모니터링/배포
│       └── .gitkeep
│
└── docs/                                ← 설계 문서 (MD 파일들)
    ├── architecture/
    ├── decisions/
    └── reports/
```
