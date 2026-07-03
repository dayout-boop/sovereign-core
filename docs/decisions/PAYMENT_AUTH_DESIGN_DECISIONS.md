# 결제 및 인증 설계 결정 v2 (Phase 1)

> **작성 기준**: 이 문서는 Sovereign Core의 핵심 원칙인 "운영자가 외부 PG 대시보드에 접속하여 수기 처리하는 방식을 완전히 배제하고, 모든 결제·환불·정산·알림 흐름을 시스템 내부 자동화 봇이 API 파이프라인으로 일관 처리"하는 아키텍처를 정의합니다. 이 문서는 다음 단계의 코드 구현 기준입니다.

---

## 1. 핵심 원칙: PG 대시보드 배제 및 API 전면 자동화

외부 PG사(Stripe, Toss 등)는 우리 시스템에서 **교체 가능한 인프라 어댑터**로만 취급합니다. 운영자가 PG사의 대시보드에 직접 로그인하여 환불이나 취소를 수기 처리하는 방식은 감사 추적이 불가능하고 자동화 파이프라인을 깨뜨리므로 **아키텍처 수준에서 금지**합니다.

모든 결제 관련 액션(환불, 취소, 구독 변경, 정산 조회)은 우리 시스템의 내부 API를 통해서만 발생하며, 이 내부 API가 PG사 API를 호출하는 단방향 흐름을 유지합니다. 운영자가 개입이 필요한 경우에도 반드시 우리 어드민 패널 → 내부 `OperatorPort` → PG 어댑터 → PG API 순서를 거칩니다.

---

## 2. 다중 PG 라우팅 파이프라인 설계

리전별로 최적화된 PG를 붙이더라도 상위 비즈니스 로직은 변경 없이 동작해야 합니다. 이를 위해 **PG 라우터(PGRouter)** 레이어를 `MultiPGPaymentAdapter` 내부에 정의합니다.

### 2-1. PG 라우팅 결정 기준

| 우선순위 | 조건 | 선택 PG |
|---|---|---|
| 1 | 테넌트의 청구 국가 = `KR` | Toss Payments |
| 2 | 테넌트의 청구 국가 = EU 회원국 | Stripe (EU 리전) |
| 3 | 그 외 글로벌 | Stripe (US 리전) |
| 4 | 특정 리전에 신규 PG 추가 시 | `PGRouterConfig`에 리전-PG 매핑 추가만으로 확장 |

### 2-2. PG 라우터 포트 설계

```
PGRouterPort
  ├── RouteByTenant(tenantID) → PGAdapterPort  // 테넌트 기준 PG 선택
  ├── RouteByRegion(region)   → PGAdapterPort  // 리전 기준 PG 선택
  └── RegisterPG(region, adapter) → void       // 신규 PG 동적 등록
```

신규 PG 추가 시 `RegisterPG`를 호출하여 라우터에 등록하는 것만으로 기존 비즈니스 로직 코드 변경 없이 확장됩니다. 이는 `MASTER_ARCHITECTURE_V1.md`의 "열린 방향성" 원칙을 준수합니다.

### 2-3. 자동화 봇과 파이프라인 흐름

```
[이벤트 발생]
    │
    ▼
[NATS JetStream 이벤트 버스]
    │
    ├─ payment.failed     → PaymentRetryBot  → PGRouterPort → PG API
    ├─ subscription.ended → CancellationBot  → PGRouterPort → PG API
    ├─ sla.violated       → SLACompensationBot → LedgerPort → 크레딧 지급
    ├─ refund.requested   → RefundBot        → PGRouterPort → PG API
    └─ settlement.due     → SettlementBot    → PGRouterPort → 정산 조회
```

각 봇은 독립적인 NATS Consumer로 동작하며, 처리 실패 시 Dead Letter Queue(DLQ)로 이동하여 재처리 또는 운영자 알림을 트리거합니다.

---

## 3. 운영자 개입 설계 (OperatorPort)

운영자의 직접 개입이 불가피한 경우(DLQ 처리, 고액 환불 승인 등)에도 반드시 내부 `OperatorPort`를 통해서만 처리합니다.

### 3-1. OperatorPort 인터페이스

```
OperatorPort
  ├── ApproveRefund(operatorID, refundID, reason) → error
  ├── ApproveCreditAdjustment(operatorID, tenantID, amount, reason) → error
  ├── ReprocessDLQ(operatorID, messageID, reason) → error
  └── GetAuditLog(tenantID, from, to) → []AuditEvent
```

### 3-2. 4-Eyes Principle 적용 범위

금액이 변경되는 모든 수기 처리는 1차 운영자 요청 후 2차 시니어 운영자 승인이 필요합니다. 이 승인 흐름도 NATS 이벤트로 처리되며, 승인 대기 중인 요청은 `operator.approval.pending` 토픽에 게시됩니다.

### 3-3. 감사 로그 불변 기록

모든 `OperatorPort` 호출은 NATS JetStream의 `audit.operator.*` 스트림에 기록됩니다. 이 스트림은 `MaxAge=7년` 정책으로 설정하여 규제 요건(PCI-DSS, 전자상거래법)을 충족합니다.

---

## 4. 이벤트 기반 알림 발송 서비스 (NotificationPort)

고객에게 발송되는 모든 알림(결제 완료, 환불 처리, 약관 변경, 구독 갱신 예정 등)은 가입 이메일을 기준으로 이벤트 버스에서 자동으로 트리거됩니다.

### 4-1. NotificationPort 인터페이스

```
NotificationPort
  ├── SendTransactional(tenantID, event NotificationEvent) → error
  │     // 결제 완료, 환불 완료, 구독 취소 등 즉시 발송
  ├── ScheduleNotification(tenantID, event, sendAt time.Time) → error
  │     // 갱신 예정 7일 전, 약관 변경 30일 전 등 예약 발송
  └── GetDeliveryStatus(notificationID) → DeliveryStatus
```

### 4-2. 알림 이벤트 유형 및 자동 트리거 매핑

| NATS 이벤트 토픽 | 알림 유형 | 발송 채널 | 발송 시점 |
|---|---|---|---|
| `payment.succeeded` | 결제 완료 영수증 | 이메일 | 즉시 |
| `payment.failed` | 결제 실패 안내 + 재시도 안내 | 이메일 + 앱 푸시 | 즉시 |
| `refund.completed` | 환불 처리 완료 | 이메일 | 즉시 |
| `subscription.renewing` | 구독 갱신 예정 안내 | 이메일 | 갱신 7일 전 |
| `subscription.cancelled` | 구독 취소 확인 | 이메일 | 즉시 |
| `terms.changing` | 약관/가격 변경 사전 고지 | 이메일 + 대시보드 배너 | 변경 30일 전 |
| `sla.compensated` | SLA 위반 크레딧 보상 안내 | 이메일 | 즉시 |
| `consent.required` | 신규 동의 수집 요청 | 이메일 + 대시보드 팝업 | 즉시 |

### 4-3. 발송 채널 어댑터 구조

`NotificationPort`의 구현체는 채널별 어댑터를 조합하여 동작합니다.

```
NotificationAdapter
  ├── EmailAdapter    (AWS SES / Resend 등 교체 가능)
  ├── PushAdapter     (FCM / APNs 추상화)
  └── InAppAdapter    (대시보드 배너/팝업 — WebSocket 또는 SSE)
```

채널 어댑터 교체 시 `NotificationPort` 인터페이스는 변경되지 않으며, 어댑터 구현체만 교체합니다.

---

## 5. 동의 수집 설계 (ConsentPort)

약관 변경 및 가격 인상 시 규제 요건을 충족하는 사전 동의를 수집합니다.

### 5-1. ConsentPort 인터페이스

```
ConsentPort
  ├── RequestConsent(tenantID, consentType, deadline time.Time) → error
  ├── RecordConsent(tenantID, consentType, accepted bool) → error
  ├── GetConsentStatus(tenantID, consentType) → ConsentStatus
  └── ListPendingConsents(tenantID) → []ConsentRequest
```

### 5-2. 지역별 규제 적용 기준

| 지역 | 규제 | 우리 시스템 적용 |
|---|---|---|
| 한국 | 전자상거래법 — 가격 인상 30일 전 사전 동의 | `ScheduleNotification` + `RequestConsent` 30일 전 자동 트리거 |
| EU | GDPR — 약관 변경 시 명시적 재동의 | `RequestConsent` 발송 후 미동의 시 갱신 차단 |
| EU | Data Act (2025.09.12) — 2개월 전 통보 후 데이터 이전 요구 가능 | `ConsentPort.RequestConsent(type=DataPortability)` 지원 |
| 미국 | FTC Click-to-Cancel — 취소를 가입만큼 쉽게 | 구독 취소 API를 가입 API와 동일한 뎁스에 노출 |

---

## 6. 환경별 인증 및 API/SDK 인증 설계

### 6-1. 클라이언트 환경별 인증 흐름

| 환경 | 인증 방식 | 비고 |
|---|---|---|
| 웹 브라우저 | JWT Bearer Token (단기 15분 + Refresh Token) | Presigned URL로 파일 직접 업로드 |
| iOS / Android 앱 | OAuth 2.0 + PKCE (Authorization Code Flow) | 필수 강제 적용 |
| 데스크탑 앱 | OAuth 2.0 + PKCE (loopback redirect URI) | |
| CLI / 헤드리스 | OAuth 2.0 Device Authorization Flow | 브라우저 없는 환경 지원 |
| 내부 서비스 간 | mTLS + 단기 JWT (15분) | Edge↔Core, 마이크로서비스 간 |

### 6-2. API Key 및 SDK 인증 설계

API Key는 데이터베이스에 **SHA-256 해시**로만 저장하며, 발급 시 평문을 1회만 노출합니다. 모든 API 요청에는 API Key를 Secret으로 사용한 **HMAC-SHA256 서명**과 타임스탬프를 포함하여 재생 공격을 방지합니다. 서버는 타임스탬프가 현재 시각 기준 ±5분을 초과하는 요청을 거부합니다.

---

## 7. 포트 확장 요약 (코드 구현 대상)

이 설계 결정을 구현하기 위해 다음 포트 및 어댑터를 추가합니다.

| 파일 | 내용 |
|---|---|
| `ports.go` 확장 | `PGRouterPort`, `OperatorPort`, `NotificationPort`, `ConsentPort` 추가 |
| `auth_ports.go` | `OAuth2PKCEPort`, `DeviceFlowPort`, `MTLSPort` 추가 |
| `adapter_pg_router.go` | 리전/테넌트 기준 PG 라우팅 어댑터 |
| `adapter_notification.go` | 이메일/푸시/인앱 알림 어댑터 (채널별 분리) |
| `adapter_consent.go` | 지역별 동의 수집 어댑터 |
| `adapter_operator.go` | 4-Eyes 승인 흐름 + 감사 로그 어댑터 |
| `bot_payment_retry.go` | 결제 실패 재시도 봇 (NATS Consumer) |
| `bot_sla_compensation.go` | SLA 위반 크레딧 보상 봇 (NATS Consumer) |
| `bot_settlement.go` | 정산 주기 봇 (NATS Consumer, 주정산 기준) |
