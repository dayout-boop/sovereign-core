# Stage 3 — 격리·보안 검증 보고서 (소프트웨어 레이어)

> 환경: Go 1.22.5 / linux-amd64. build rc=0, vet rc=0.
> 전체 테스트 30개 중 **29 PASS / 1 FAIL(의도된 결함 재현)**.
> 하드웨어(MicroVM/gVisor) 없이 검증 가능한 소프트웨어 레이어 전 시나리오.

---

## 1. 결과 요약

| 시나리오 | 공격 방법 | 결과 | 비고 |
|---|---|---|---|
| **T9-A 토큰 위조** | `mock.{base64(OrgID)}` 직접 조작 | **FAIL — 결함 재현** | MockAuth 설계 한계 |
| T9-B 교차 테넌트 엔드포인트 조회 | orgA 토큰으로 orgB ep 조회 | PASS — 차단 | store RLS 동작 |
| T9-C 교차 테넌트 오퍼레이션 폴링 | orgA 토큰으로 orgB op 폴링 | PASS — 차단 | store RLS 동작 |
| T9-D 교차 테넌트 엔드포인트 정지 | orgA 토큰으로 orgB ep suspend | PASS — 차단 | "endpoint not found" |
| T9-E 토큰 없이 보호 라우트 | Authorization 헤더 없음 | PASS — 401 | 미들웨어 동작 |
| T9-F 빈/공백 Bearer | `Bearer `, `Bearer  ` 등 | PASS — 401 | 4종 전부 거부 |
| T9-G org 클레임 없는 토큰 | OrgID 없는 Claims 인코딩 | PASS — 401 | missing org claim |
| T9-H 미등록 endpointID Resolve | 존재하지 않는 ep | PASS — 에러 반환 | "not found" |
| **T9-I SNI 경계 설계 갭** | 다른 테넌트 ep를 SNI에 삽입 | **PASS — 설계 갭 기록** | 프록시 레이어 RLS 필요 |
| T9-J 동시 100건 교차 테넌트 | goroutine 100개 동시 공격 | PASS — 누출 0건 | race clean |

---

## 2. T9-A 결함 — MockAuth 토큰 위조 취약점

### 재현 방법

```
mock.eyJPcmdJRCI6ImV2aWwifQ==
= "mock." + base64("{"OrgID":"evil"}")
```

`MockAuth.VerifyToken`은 `mock.` 접두어 + 유효한 base64 + OrgID 존재 여부만 검사한다. 따라서 누구든 원하는 OrgID를 base64로 인코딩해 `mock.` 접두어를 붙이면 **임의 org로 인증이 통과**된다.

### 심각도

**Critical(초기 트랙 한정).** 현재 MockAuth는 테스트용 가짜 IdP이므로 프로덕션에서는 이 코드가 사용되지 않는다. 그러나 **진짜 OIDC IdP(서명 검증 JWT)로 교체하기 전까지 이 취약점이 실제로 존재**한다. 초기 트랙에서 MockAuth를 그대로 두고 서버를 노출하면 임의 테넌트 사칭이 가능하다.

### 수정 방향

`MockAuth`를 실제 서버에 사용하지 않는 것이 원칙. 초기 트랙 실물 배선 시 반드시 서명 검증 JWT(HMAC-SHA256 또는 RSA/ECDSA JWKS)로 교체해야 한다. 테스트 환경에서도 `mock.` 접두어 외에 **서명 검증 단계**를 추가하면 이 취약점을 막을 수 있다.

---

## 3. T9-I 설계 갭 — RoutingRegistry의 org 미검증

### 발견 내용

```
SNI="ep_orgA_001.ap-northeast-2.internal" → epID=ep_orgA_001(method=sni)
Resolve: addr=10.0.0.1:5432 err=<nil>
```

`RoutingRegistry.Resolve`는 endpointID가 레지스트리에 존재하면 **연결 요청자의 org를 검증하지 않고** 백엔드 주소를 반환한다. 실제 프록시(TLS 터미네이션 레이어)에서 연결자 org와 `ep.OrgID`를 대조하는 추가 RLS가 없으면, SNI에 다른 테넌트의 endpointID를 삽입해 **해당 테넌트의 DB 백엔드로 직접 라우팅**될 수 있다.

### 심각도

**High(데이터플레인 레이어).** 현재 제어평면 HTTP API는 `c.OrgID`로 RLS가 걸려 있지만, `RoutingRegistry`는 데이터플레인(Postgres 연결 프록시) 경로이므로 별도 RLS가 필요하다.

### 수정 방향

`Resolve(endpointID, callerOrgID string)` 시그니처로 변경하고, `ep.OrgID != callerOrgID`이면 에러 반환. 또는 프록시 레이어에서 TLS 클라이언트 인증서(mTLS) 또는 JWT 클레임으로 callerOrgID를 추출해 대조.

---

## 4. 통과한 방어 메커니즘 정리

| 방어 계층 | 동작 방식 | 검증 결과 |
|---|---|---|
| **HTTP 인증 미들웨어** | `Bearer` 토큰 → `VerifyToken` → 401 | T9-E/F/G 전부 차단 |
| **store RLS(getEndpoint/opSnapshot)** | `(orgID, resourceID)` 쌍 조회 — 다른 org의 ID는 "없는 것처럼" | T9-B/C/D/J 전부 차단 |
| **라우팅 레지스트리 미등록 거부** | 등록되지 않은 endpointID → 에러 | T9-H 차단 |
| **동시성 안전** | 100 goroutine 동시 교차 공격 → 누출 0, race clean | T9-J |

---

## 5. 누적 검증 현황 (전체 30개)

| 그룹 | 테스트 수 | PASS | FAIL(의도) |
|---|---|---|---|
| Stage 2 정산 정합성 (T1~T5) | 5 | 5 | 0 |
| Stage 2 실물 스토리지 (T6 A~E) | 5 | 5 | 0 |
| Stage 2 잔액 원장 (T7 A~D) | 4 | 4 | 0 |
| Stage 2 결제 마진 (T8 A~F) | 6 | 6 | 0 |
| **Stage 3 보안 레드팀 (T9 A~J)** | **10** | **9** | **1(T9-A 결함 재현)** |
| **합계** | **30** | **29** | **1** |

---

## 6. 하드웨어 확보 후 추가 검증 필요 항목 (Stage 3 잔여)

소프트웨어 레이어에서 검증 불가능한 항목들:

| 항목 | 필요 환경 |
|---|---|
| MicroVM 메모리 격리(cgroup v2, seccomp) | Firecracker 실행 가능한 베어메탈 |
| 컨테이너 탈출 시도(CVE 재현) | 격리된 테스트 VM |
| 만료 Presigned URL 재사용 | 실제 오브젝트 스토리지(S3/Vultr) |
| eBPF 감사로그 기록 확인 | Linux 5.8+ 커널 + BPF 권한 |

---

## 7. 다음 단계 (권장)

1. **T9-A 수정 결정:** MockAuth → 서명 검증 JWT 교체 일정 확정. 초기 트랙 실물 배선 전 필수.
2. **T9-I 수정 결정:** `Resolve`에 callerOrgID 파라미터 추가 또는 프록시 mTLS 검증.
3. **Stage 4(통합 부하):** 하드웨어 확보 후 k6/Locust로 30,000→50,000 RPS 램프업. Stage 2/3 조건이 부하 중에도 유지되는지 검증.
