# 코드 갭 감사 보고서 (2026-07-03)

## 현재 상태 요약

빌드 상태: **BUILD OK** (컴파일 오류 0건)
테스트 상태: **39/39 PASS** (race detector 포함, FAIL 0건)

---

## 설계 결정 대비 미구현 항목 전수 목록

아래는 `PAYMENT_AUTH_DESIGN_DECISIONS.md` v2에 정의된 포트·어댑터·봇 중 현재 코드에 **존재하지 않는** 항목입니다. 이 항목들을 일괄 구현할 때 발생할 수 있는 위험도를 함께 기록합니다.

### 미구현 포트 (ports.go / auth_ports.go 에 추가 필요)

| 포트 이름 | 위험도 | 위험 원인 |
|---|---|---|
| `PGRouterPort` | **중** | `MultiPGPaymentAdapter` 내부 구조 변경 필요. 기존 `FullPaymentPort` 컴파일 계약 유지 필수. |
| `OperatorPort` | **낮음** | 신규 추가. 기존 코드와 인터페이스 충돌 없음. |
| `NotificationPort` | **낮음** | 신규 추가. 기존 코드와 인터페이스 충돌 없음. |
| `ConsentPort` | **낮음** | 신규 추가. 기존 코드와 인터페이스 충돌 없음. |
| `OAuth2PKCEPort` | **낮음** | 신규 추가. 기존 `AuthPort`와 별도 파일로 분리. |
| `DeviceFlowPort` | **낮음** | 신규 추가. |
| `MTLSPort` | **낮음** | 신규 추가. |

### 미구현 어댑터 (신규 파일 추가 필요)

| 파일명 | 위험도 | 위험 원인 |
|---|---|---|
| `adapter_pg_router.go` | **중** | `MultiPGPaymentAdapter` 내부 PG 선택 로직 리팩토링 필요. 기존 테스트 깨질 가능성 있음. |
| `adapter_notification.go` | **낮음** | 신규 파일. 기존 코드 무변경. |
| `adapter_consent.go` | **낮음** | 신규 파일. 기존 코드 무변경. |
| `adapter_operator.go` | **낮음** | 신규 파일. 기존 코드 무변경. |

### 미구현 봇 (신규 파일 추가 필요)

| 파일명 | 위험도 | 위험 원인 |
|---|---|---|
| `bot_payment_retry.go` | **낮음** | 신규 파일. `EventBusPort` 기존 구현 활용. |
| `bot_sla_compensation.go` | **낮음** | 신규 파일. `Ledger` 기존 구현 활용. |
| `bot_settlement.go` | **낮음** | 신규 파일. `EventBusPort` 기존 구현 활용. |

### app.go 의존성 주입 누락

`App` 구조체에 신규 포트 필드가 추가되어야 합니다. 현재 `payment CustomerPort`로만 주입되어 있으며, `NotificationPort`, `ConsentPort`, `OperatorPort`, `PGRouterPort`가 누락된 상태입니다.

---

## 일괄 구현 시 위험도 평가

### 위험 요소 분류

**높음 (즉시 주의 필요):**
없음. 현재 빌드 및 테스트가 완전히 통과 중이며, 신규 포트는 모두 독립적으로 추가 가능합니다.

**중간 (신중한 구현 필요):**
`PGRouterPort` 및 `adapter_pg_router.go` — 기존 `MultiPGPaymentAdapter`의 내부 PG 선택 로직을 외부 라우터로 분리하는 리팩토링이 필요합니다. 기존 `FullPaymentPort` 컴파일 계약(`var _ FullPaymentPort = (*MultiPGPaymentAdapter)(nil)`)을 유지하면서 내부 구조를 변경해야 하므로, 기존 테스트 39개가 모두 통과하는지 확인하며 단계적으로 진행합니다.

**낮음 (안전하게 추가 가능):**
나머지 6개 포트 및 5개 어댑터/봇은 모두 신규 파일 추가이며, 기존 코드를 수정하지 않습니다. 각 파일에 `var _ XxxPort = (*XxxAdapter)(nil)` 컴파일 계약을 선 배치하면 타입 불일치를 빌드 시점에 즉시 감지할 수 있습니다.

### 결론

일괄 구현 시 오타나 타입 불일치로 인한 런타임 오류 위험은 **낮습니다.** Go의 컴파일 타임 인터페이스 검증(`var _ Port = (*Adapter)(nil)` 패턴)을 모든 신규 어댑터에 적용하면, 인터페이스 계약 위반은 빌드 단계에서 즉시 차단됩니다. 단, `PGRouterPort` 관련 리팩토링은 기존 테스트를 기준선으로 삼아 단계적으로 진행해야 합니다.

---

## 권장 구현 순서

1. `ports.go` 확장 → `PGRouterPort`, `OperatorPort`, `NotificationPort`, `ConsentPort` 추가
2. `auth_ports.go` 신규 → `OAuth2PKCEPort`, `DeviceFlowPort`, `MTLSPort` 추가
3. `adapter_notification.go` → `NotificationPort` Mock 구현 + 컴파일 계약
4. `adapter_consent.go` → `ConsentPort` Mock 구현 + 컴파일 계약
5. `adapter_operator.go` → `OperatorPort` Mock 구현 + 4-Eyes 흐름 + 컴파일 계약
6. `adapter_pg_router.go` → `PGRouterPort` 구현 + `MultiPGPaymentAdapter` 내부 리팩토링
7. `bot_*.go` 3개 → NATS Consumer 봇 구현
8. `app.go` 확장 → 신규 포트 필드 및 의존성 주입 추가
9. 전체 테스트 실행 → 기존 39개 + 신규 테스트 전부 PASS 확인
