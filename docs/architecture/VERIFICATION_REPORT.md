# 검증 보고서 — Sovereign Core 컨트롤플레인 Go 패키지 (v0.2 · 웜풀 추가)

**대상:** Go 모듈 `sovereigncore/controlplane` (단일 `package main`, 소스 9개 + `warmpool.go` 신규, stdlib only)**작업 범위:** 빌드·실행·스모크/레이스 검증, 컴파일 오류만 수정(로직 불변), 지정 형식 보고**결론:** 9개 항목 전부 기대치 충족. 컴파일을 막는 오류 **1건만 최소 수정**(메서드명 1곳), 로직·구조·출력 불변. 레이스 **0건(클린)**.

---

## 0. 환경 확인 — `go version`

```
go version go1.22.5 linux/amd64
```

요구 사항(Go 1.22+, HTTP 라우팅 패턴 `POST /path/{id}`) 충족. 작업 디렉터리 = `go.mod` 위치(`~/sovereign_core`). `gcc 13.3.0` 설치(레이스 디텍터용 cgo).

---

## (a) `go vet ./...` — 정적 검사

```
(출력 없음)
종료코드: 0
```

**판정: 통과** (경고 없음). *수정 적용 후 결과* — 수정 전에는 컴파일 오류로 vet 실패(아래 "수정한 내용" 참조).

---

## (b) `go build ./...` — 컴파일

```
(출력 없음)
종료코드: 0
```

**판정: 성공** (수정 적용 후). 외부 의존성 없이 stdlib만으로 컴파일 완료.

---

## (c) `go run .` — 자체 점검 모드

전체 출력:

```
── Sovereign Core · P0 루프 mock 관통 (웜풀 포함) ──
  [0] 웜풀 정책=load-proportional(buf=1.5,min=2,max=64), 사전부팅 준비=2개
  [1] signup → org=org_bO8mw2RYWSRo plan=free kek=... billing=...
  [2] verify token → org_id=org_bO8mw2RYWSRo role=owner (RLS 경계)
  [3] create project(async) → project=proj_... root_branch=br_...
  [4] create branch(CoW from br_...) → branch=br_...
  [5] start endpoint → endpoint=ep_nnLJYleAZRz3 warm_hit=true boot=0ms
  [5]        connection_uri = postgresql://...@ep_nnLJYleAZRz3.ap-northeast-2.internal:5432/main?sslmode=require
  [6] SNI 라우팅: ep_....internal → endpoint=ep_... backend=inst_....compute.internal boot=0ms
  [✓] endpoint 식별 3중 폴백: SNI=options=password 동일 도달
  [7] usage rollup = map[branch_ops:2 cu_hours:0]
  [8] suspend endpoint → state=suspended (scale-to-zero)
  [8] 재연결 → WakeHook backend=inst_....compute.internal boot=0ms
  [9] drain 후 기동 → warm_hit=false boot=150ms (미스=콜드부팅)
  [9] 웜풀 통계: 준비=4 히트=2 미스=1 정책=load-proportional(buf=1.5,min=2,max=64)
  [✓] RLS 경계: 타 org 접근 차단 확인
  [✓] 멱등성: 동일 키 재요청 → 동일 op
  [✓] 상태머신: 금지 전이(deleted→active) 거부 확인
  [✓] 웜풀: 사전부팅 히트(boot=0) + drain 후 미스(boot>0) 확인
── 관통 완료: signup→project→branch→endpoint→connection_uri→usage→suspend ──
   (mock. 진짜 교체: mocks.go MockStorage.BootInstance/AttachBranch 만 엔진 호출로.)
종료코드: 0
```

합격 토큰 충족 여부:

| 합격 조건 | 결과 | 충족 |
| --- | --- | --- |
| `[✓]` 로 시작하는 줄 정확히 **5개** | 5개 | 충족 |
| 마지막 부근 `관통 완료` 포함 줄 1개 | 존재 | 충족 |
| panic / FAIL / 비정상 종료 없음 | 종료코드 0 | 충족 |

**판정: 통과.** (ID·시크릿류는 실행마다 무작위 생성 — 보고서에선 자격증명 값 일부 마스킹. 웜풀 히트 시 `boot=0ms`, drain 후 미스 시 `boot=150ms`로 실측 거동 확인.)

---

## (d) HTTP 서버 모드 스모크 테스트 — `go run . -serve`

기동 로그:

```
Sovereign Core control-plane (mock) :8080
```

`:8080` 대기 확인(기동 성공). curl 4건 결과:

| # | 요청 | 기대 | 실제 응답코드 | 본문 핵심 | 충족 |
| --- | --- | --- | --- | --- | --- |
| 1 | `POST /v1/auth/signup` | 201 + `token` | **201 Created** | `{"org":{...},"token":"mock.eyJ..."}` | 충족 |
| 2 | `POST /v1/projects` (`Idempotency-Key: key-123`) | 202 + `operation_id` | **202 Accepted** | `{"operation_id":"op_yDRj6Zh9eRRA"}`, `Location: /v1/operations/op_yDRj6Zh9eRRA` | 충족 |
| 3 | 동일 키 재요청 | 202 + `idempotent_replay` | **202 Accepted** | `{"idempotent_replay":"true","operation_id":"op_yDRj6Zh9eRRA"}` (동일 op) | 충족 |
| 4 | `POST /v1/projects` (토큰 없음) | 401 | **401 Unauthorized** | `{"error":"unauthorized"}` | 충족 |

**판정: 4건 전부 기대치 충족.** 테스트 후 서버 프로세스 정상 종료.

---

## (e) 레이스 검사 — `go run -race .`

```
(WARNING: DATA RACE 없음)
관통 완료 출력 후 종료
종료코드: 0
```

**판정: 클린(레이스 0건).** 이번 버전은 비동기 op 완료를 잠금 하에서 처리(`store.go`의 `completeOp`/`failOp`/`opSnapshot`이 `RWMutex` 보호)하도록 변경되어, 이전 v0.1에서 보고했던 16건의 데이터 레이스가 해소되었습니다. 요청서의 기대치("0건이 정상")와 일치하며, 회귀를 의심할 경고는 출력되지 않았습니다.

---

## 수정한 내용

**1건 수정 (컴파일 오류 — 메서드명 불일치).**

| 파일 | 줄 | 변경 전 | 변경 후 |
| --- | --- | --- | --- |
| `handlers.go` | 131 | `op, ok := s.app.store.getOperation(c.OrgID, r.PathValue("oid"))` | `op, ok := s.app.store.opSnapshot(c.OrgID, r.PathValue("oid"))` |

diff:

```
 func (s *Server) handleGetOperation(w http.ResponseWriter, r *http.Request, c Claims ) {
-	op, ok := s.app.store.getOperation(c.OrgID, r.PathValue("oid"))
+	op, ok := s.app.store.opSnapshot(c.OrgID, r.PathValue("oid"))
 	if !ok { writeJSON(w, 404, map[string]string{"error": "not found"}); return }
 	writeJSON(w, 200, op)
 }
```

**사유 및 규칙 부합성:**

- `handlers.go`는 이번에 갱신본이 제공되지 않아 옛 메서드명 `getOperation`을 그대로 참조했으나, 갱신된 `store.go`에는 해당 메서드가 없고 **잠금 보호 스냅샷 메서드 ****`opSnapshot(orgID, id string) (Operation, bool)`** 로 대체되어 있었습니다. → `undefined: store.getOperation` 컴파일 실패.

- 두 메서드는 **시그니처가 동일**(`(orgID, id string) (Operation, bool)`)하여 호출명만 교체하면 동작·반환·조건 분기·출력 어느 것도 바뀌지 않습니다.

- 4번 규칙의 허용 항목("컴파일을 막는 오류만 — 오타·타입 불일치·메서드명 수준")에 정확히 해당하며, 판단 기준("안 고치면 컴파일 실패?" → 예)을 충족합니다. 그 외 파일·로직·구조·gofmt 변경은 없습니다.

> 그 외 8개 파일 및 `warmpool.go` 신규 파일은 무수정. 파일 추가·삭제·이름변경 없음.

---

## 한 줄 요약

Go 1.22.5 환경에서 컴파일을 막던 메서드명 불일치 1건(`handlers.go:131` `getOperation`→`opSnapshot`, 시그니처 동일·로직 불변)만 최소 수정한 뒤, `go vet`(통과)·`go build`(성공)·`go run .`(합격 토큰 `[✓]`×5 및 `관통 완료`, 종료코드 0)·`-serve` 스모크(201/202/202+replay/401 전부 충족)·`go run -race .`(**레이스 0건 클린**, 종료코드 0)을 모두 확인했습니다.

