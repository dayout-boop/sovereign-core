# Sovereign Core — 코드 레포 (축 B)

**이 레포 = 축 B ("어떻게 돈다").** 설계·근거·결정은 축 A(문서 00~99)에 있고, 여기엔 실행 구현체만 둔다. 둘을 섞지 않는다 (00 인덱스 §1).

- 레포 시작: 2026-07-02, P0-(d) 해소 — 기존 레포 없음 확인 → 신규 시작으로 확정 (90 §5 분기)
- Go 1.22, stdlib-only (현 단계)

## 구조 (00 인덱스 축 B 규칙)

```
/src          애플리케이션 코드
  /governance   66 — change pipeline·경계 sim        ✅ 코드 있음
  /training     53 — 개발엔진                          ✅ 코드 있음
  /brain        50 — C 운영두뇌                        (예정)
  /state        22·61 — 상태저장소·생명주기            (예정)
  /auth         26 — 인증·권한                         (예정)
  /security     25 — 키·시크릿                         (예정)
  /queue        24·51 — 전단 배치커밋 큐               (예정)
  /orchestrator 52·60 — D 다중연결·거점                (예정)
  /proxy        62 — 라우팅·격리 폐루프                (예정)
  /inference    27 — B GPU                             (예정)
/infra        IaC (Terraform·CAPI·cloud-init)          (예정)
/migrations   DDL·RLS SQL                              (예정)
/skills       운영 스킬·런북                           (예정)
```

## 현재 코드 (시뮬 검증분, 2026-07-01 세션)

| 경로 | 내용 | 검증 |
|---|---|---|
| src/governance/change_pipeline | 7단 게이트 **v0.2** — 게이트3-0 구조 하드레일(개발↛인프라, D52/66 v0.3) 추가 | vet·build·run·-race clean, 8케이스 관통 |
| src/governance/engine_boundary_sim | 개발/운영 경계 3문(Q1 GPU 단일풀 D53·Q2 purpose 분기 D54·Q3 우회차단 D52) | run 3문 ✓ |
| src/training/dev_engine_flow | 개발엔진 실전 흐름(맥락·판단·CRUD·파이프라인 경유) | run ✓ |

## 실행

```
go vet ./... && go build ./...
go run ./src/governance/change_pipeline/
go run ./src/governance/engine_boundary_sim/
go run ./src/training/dev_engine_flow/
```

## 규칙

1. 설계 변경은 문서(축 A) 먼저 — 어긋나면 문서 우선 (00 §7)
2. 하드레일은 어댑터가 아닌 파이프라인 본체에 — mock/실구현 교체와 무관하게 강제
3. LLM = Proposer만. 판정(결정론)·집행(상태기계) 인터페이스 없음
