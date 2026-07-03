# Sovereign Core — 컨트롤플레인 워킹스켈레톤 (수직 슬라이스 + 웜풀)

**무엇:** P0 루프(`signup→project→branch→endpoint→connection_uri→usage→suspend`) +
SNI 라우팅 + **웜풀 기반 컴퓨트 기동**을 엔진·AWS 없이 한 바퀴 도는 Go 스켈레톤.

## 실행
```bash
go run .            # walkthrough (웜풀 히트/미스 포함 관통 출력)
go run . -serve     # HTTP 서버 :8080 (REST /v1)
```

## 파일 (단일 package main, stdlib only)
| 파일 | 역할 |
|---|---|
| `model.go` | 도메인 타입 + 노드 상태머신(허용 전이만) |
| `ports.go` | 포트 인터페이스 — Storage/Auth/Kms/Secret/Payment. **Storage는 BootInstance(콜드부팅)+AttachBranch(부착) 분리** |
| `mocks.go` | mock 어댑터. 진짜 교체 시 이 파일만 |
| `store.go` | 인메모리 metaDB + RLS 시뮬 + **잠금 하 비동기 op 완료**(레이스 없음) |
| `warmpool.go` | **웜풀 + 선택형 정책**(Fixed / LoadProportional). 히트/미스 계측 |
| `app.go` | P0 로직 + 웜풀/라우팅 배선. 컴퓨트 기동=웜풀 Acquire |
| `handlers.go` | REST `/v1` — JWT→org_id(RLS)→멱등성→202+op |
| `routing.go` | SNI/options/password 폴백 + WakeHook(scale-to-zero) |
| `main.go` | walkthrough / `-serve` |

## 웜풀 정책 (다중 선택)
컴퓨트를 미리 부팅해두고(히트→boot 0), 풀이 비면 콜드부팅(미스→실측). 정책은 인터페이스:
- **LoadProportional**(기본, 추천): 최근 생성률 × 버퍼, [Min,Max] 클램프. 에이전트 버스트에 적응.
- **Fixed**: 상시 N개. 단순·예측가능.

교체:
```go
app := NewAppWithPolicy(FixedPolicy{N: 8})
// 기본: NewApp() == NewAppWithPolicy(LoadProportionalPolicy{Buffer:1.5, Min:2, Max:64})
```
실제 계수는 PoC 데이터로 확정. 골격은 인터페이스만 고정.

## 증명된 것 (walkthrough)
- 7개 제품 기반 연결: 테넌트·인증·과금·플랜·상태머신·접속면·미터링
- SNI 라우팅 + 식별 3중 폴백(SNI=options=password 동일 도달)
- 웜풀 히트(boot=0) / drain 후 미스(boot>0) + 통계
- RLS 경계 / 멱등성 / 상태머신 금지전이
- **잠금 하 비동기 완료 → `go run -race` 클린(레이스 0)**

`connection_uri` host = `{endpoint_id}.{region}.internal` (SNI 키). 실제 백엔드 = 웜풀 인스턴스 host(라우팅 레지스트리 매핑).

## mock → 진짜 교체
| 포트 | 진짜 |
|---|---|
| StoragePort.BootInstance | 자체 엔진 콜드부팅(PVM/Firecracker) — **boot_ms 실측 지점** |
| StoragePort.AttachBranch | 데워둔 컴퓨트에 브랜치 부착 |
| AuthPort | 서명 JWT + JWKS (Keycloak/Ory) |
| KmsPort | AWS KMS / SecretPort | Vault / PaymentPort | Stripe |

## 빌드 주의 (작성 환경)
작성 환경에 Go 툴체인 부재 → 여기선 컴파일 못 함. 동일 로직을 미러로 돌려 시퀀스·상태머신·RLS·멱등성·웜풀 히트/미스를 확인함. 당신 환경에서 `go run .` 권장(단일 package, 외부 의존 0).
