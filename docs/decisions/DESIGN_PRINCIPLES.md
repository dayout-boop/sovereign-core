# DESIGN_PRINCIPLES.md — 코드 작성 원칙

> **제정일**: 2026-07-03
> **적용 범위**: 이 날짜 이후 모든 코드 작업에 강제 적용.
> **위반 시**: 즉시 작업 중단 후 사용자 보고.

---

## 원칙 1: 코드 설계서 → 기술 검토 → 코드 순서 강제

**코드를 짜기 전에 반드시 코드 설계서를 먼저 작성하고 사용자 확인을 받는다.**

```
[단계 1] 코드 설계서 작성 (DESIGN_SPEC_*.md)
    ↓ 사용자 확인
[단계 2] 연계 기술 및 최신 기술 검토 (외부 검색 포함)
    ↓ 사용자 확인
[단계 3] 포트(인터페이스) 정의
    ↓ 빌드 검증
[단계 4] L2 Stub 구현 (ErrNotProvisioned 반환, return nil 금지)
    ↓ 빌드 + 테스트 검증
[단계 5] 인프라 전제조건 충족 후 L3 실제 구현
```

이 순서를 건너뛰는 것은 금지한다.

---

## 원칙 2: 코드 설계서(DESIGN_SPEC_*.md) 필수 포함 항목

코드를 짜기 전에 작성하는 설계서는 다음 항목을 반드시 포함한다.

| 항목 | 내용 |
|------|------|
| **도메인 언어 정의** | 이 코드에서 사용하는 용어의 정확한 의미. 기술 용어와 비즈니스 용어를 구분 |
| **포트 메서드 목록** | 메서드명, 파라미터, 반환값, 비즈니스 의도 (기술 구현 방식 기재 금지) |
| **에러 타입 정의** | 도메인별 에러 코드 목록. 범용 에러 타입 사용 금지 |
| **인프라 전제조건** | 이 코드가 실제 동작하기 위해 필요한 인프라 목록 |
| **L2 Stub 동작 정의** | 인프라 없을 때 각 메서드가 반환할 에러 코드 |
| **연계 기술 목록** | 사용할 외부 라이브러리, OSS, 클라우드 서비스 목록 및 도입 시점 |
| **구현 계층 매핑** | 각 메서드가 L2 Stub인지 L3 실제 구현인지 명시 |

---

## 원칙 3: 포트(인터페이스) 설계 규칙

**포트는 비즈니스 의도만 표현한다. 기술 구현 방식은 포트에 노출하지 않는다.**

| 금지 | 허용 |
|------|------|
| `WALLag int64` | `SyncLagBytes int64` |
| `PhiScore float64` | `FailureScore float64` |
| `BackupTypeWAL` | `BackupTypeContinuous` |
| `PKCEChallenge` 구조체를 포트 파라미터로 사용 | `AuthorizeRequest` 구조체 (기술 중립) |
| `func(nodeID string, phi float64)` 콜백 | `func(nodeID string, reason FailoverReason)` 콜백 |

**포트 메서드 하나 = 비즈니스 행위 하나.**

---

## 원칙 4: `return nil` 완전 금지

**구현되지 않은 메서드는 반드시 도메인 에러를 반환한다.**

```go
// 금지
func (s *ClusterStub) MakeNodeActive(ctx context.Context, nodeID string) error {
    return nil // 금지: 성공처럼 보이지만 아무것도 안 함
}

// 허용
func (s *ClusterStub) MakeNodeActive(ctx context.Context, nodeID string) error {
    return &ClusterError{
        Code:  ErrClusterNotProvisioned,
        Cause: fmt.Errorf("L3 미배포: 인프라 프로비저닝 후 NeonClusterAdapter로 교체"),
    }
}
```

---

## 원칙 5: 테스트는 포트 계약만으로 작성

**테스트가 어댑터 내부 구조체를 직접 접근하는 것은 금지한다.**

```go
// 금지 — 어댑터 내부 맵 직접 조작
a.mu.Lock()
a.sessions["test_code"] = &pkceSession{...}
a.mu.Unlock()

// 허용 — 포트 메서드만 사용
authURL, state, err := port.AuthorizeURL(ctx, clientID, redirectURI, scope, req)
token, err := port.ExchangeCode(ctx, code, verifier, redirectURI)
```

테스트에서 내부 구조체 접근이 필요하다면, 그것은 포트 설계가 잘못됐다는 신호다.

---

## 원칙 6: 외부 라이브러리 도입 시점 규칙

**외부 라이브러리는 인프라 전제조건이 충족된 후에만 도입한다.**

| 라이브러리 | 도입 시점 | 전제조건 |
|-----------|-----------|---------|
| `etcd-io/raft` | 인프라 후 | 물리 서버 3대 + 네트워크 구성 완료 |
| `neon SDK` | 인프라 후 | Neon 엔진 배포 완료 |
| `velero client` | 인프라 후 | MinIO + Velero 배포 완료 |
| `stripe-go` | 결제 연동 후 | Stripe 계정 + API Key 발급 |
| `opa/rego` | AuthZ 구현 후 | OPA 서버 배포 완료 |

**인프라 없이 외부 라이브러리를 import하면 다시 인메모리 Mock으로 우회하게 된다. 이것은 금지한다.**

---

## 원칙 7: 문서 생성 규칙

| 문서 유형 | 작성 시점 | 파일명 패턴 |
|-----------|-----------|------------|
| **코드 설계서** | 코드 작성 전 | `DESIGN_SPEC_[도메인명].md` |
| **기술 검토서** | 설계서 작성 후, 코드 전 | `TECH_REVIEW_[도메인명].md` |
| **구현 매트릭스** | 포트 정의 후 | `IMPL_MATRIX.md` (단일 파일, 지속 업데이트) |
| **알려진 문제** | 동결 선언 시 | `KNOWN_ISSUES.md` |
| **검증 보고서** | 구현 완료 후 | `VERIFICATION_[도메인명].md` |

**조사 보고서, 진행 스냅샷, 사후 설계 문서는 더 이상 생성하지 않는다.**

---

## 원칙 8: 같은 파일 3회 수정 금지

같은 파일의 같은 부분을 3회 수정하는 경우 즉시 작업을 중단하고 사용자에게 보고한다.
컴파일 오류가 3회 반복되면 즉시 작업을 중단하고 사용자에게 보고한다.
