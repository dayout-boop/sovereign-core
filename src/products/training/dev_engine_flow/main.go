// Sovereign Core — 개발엔진 실전 요청처리 시뮬 (dev_engine_flow.go)
//
// 목적: 사용자가 "이거 고쳐줘/만들어줘"라고 하면 개발엔진(53)이
//   ① 이전 대화 맥락을 조회(memory)
//   ② 무엇을 손볼지 판단 (운영엔진 코드 / OSS 개조 / 레이어별 언어)
//   ③ code 생성·수정 (생성물 CRUD)
//   ④ 반드시 파이프라인 경유 (즉시 집행 X) — 앞 경계 유지
// 를 어떻게 처리하는지 코드로 검증.
//
// 앞 engine_boundary_sim의 경계(개발↛인프라, purpose 분기) 위에 실전 흐름을 얹음.
// stdlib-only.

package main

import (
	"errors"
	"fmt"
	"strings"
)

// ── 사용자 요청 (개발 성격) ──────────────────────────────────────────

type DevRequest struct {
	ID       string
	UserID   string
	Utterance string // 자연어 요청 (LLM이 파싱)
	SessionID string // 이전 대화 맥락 키
}

// ── ① 맥락 조회 (memory / 이전 대화) ─────────────────────────────────

type ConversationMemory struct {
	store map[string][]string // sessionID → 이전 turn 요약
}

func (m *ConversationMemory) Recall(sessionID string) []string {
	if v, ok := m.store[sessionID]; ok {
		return v
	}
	return nil // 맥락 없음 = 새 대화
}
func (m *ConversationMemory) Append(sessionID, turn string) {
	m.store[sessionID] = append(m.store[sessionID], turn)
}

// ── ② 대상 판단 (무엇을 손보나) ─────────────────────────────────────

type Target struct {
	Layer    string // control-plane / proxy / queue / oss-model / infra-iac
	Lang     string // go / rust / sql / python / hcl
	Kind     string // modify-ops / patch-oss / new-feature
	Forbidden bool  // 인프라 직접 변경 등 = 개발엔진 금지 대상
}

// 요청 의도 → 대상 레이어·언어 매핑 (실제론 code LLM이 판단)
func classifyTarget(utterance string, ctx []string) Target {
	u := strings.ToLower(utterance)
	switch {
	case strings.Contains(u, "라우팅") || strings.Contains(u, "운영엔진"):
		return Target{"control-plane", "go", "modify-ops", false}
	case strings.Contains(u, "큐") || strings.Contains(u, "배치커밋"):
		return Target{"queue", "rust", "modify-ops", false} // 임계경로=Rust
	case strings.Contains(u, "쿼리") || strings.Contains(u, "스키마"):
		return Target{"proxy", "sql", "modify-ops", false}
	case strings.Contains(u, "oss") || strings.Contains(u, "모델 개조"):
		return Target{"oss-model", "python", "patch-oss", false}
	case strings.Contains(u, "인프라") || strings.Contains(u, "노드 직접"):
		return Target{"infra-iac", "hcl", "modify-ops", true} // ⛔ 개발엔진 금지
	default:
		return Target{"control-plane", "go", "new-feature", false}
	}
}

// ── ③ code 생성·수정 (생성물 CRUD) ─────────────────────────────────

type Artifact struct {
	Path string
	Lang string
	Op   string // create / read / update
	Diff string
}

func generateCode(t Target, req DevRequest, ctx []string) (Artifact, error) {
	if t.Forbidden {
		return Artifact{}, errors.New("개발엔진은 인프라(IaC) 직접 변경 금지 — 운영엔진(50) reconcile 영역")
	}
	op := "update"
	if t.Kind == "new-feature" {
		op = "create"
	}
	// 맥락을 반영한 diff (이전 대화가 있으면 이어서)
	ctxNote := "신규"
	if len(ctx) > 0 {
		ctxNote = fmt.Sprintf("이전맥락 %d턴 반영", len(ctx))
	}
	return Artifact{
		Path: t.Layer + "/" + t.Lang + "_change",
		Lang: t.Lang,
		Op:   op,
		Diff: fmt.Sprintf("[%s] %s (%s)", t.Lang, req.Utterance, ctxNote),
	}, nil
}

// ── ④ 반드시 파이프라인 경유 (앞 경계 유지) ─────────────────────────

func toPipeline(a Artifact) []string {
	return []string{
		"DevEvalSuite(judge): 코드 품질·회귀 채점",
		"게이트3: 개발엔진↛인프라 하드레일",
		"shadow → 카나리 → (비가역이면) 오너 천장",
		"반영: 운영엔진(50)이 새 버전 배포 — 개발은 여기서 손 뗌",
	}
}

// ── 개발엔진 전체 처리 ──────────────────────────────────────────────

func handleDevRequest(req DevRequest, mem *ConversationMemory) {
	fmt.Printf("── [%s] user=%s :: %q\n", req.ID, req.UserID, req.Utterance)

	// ① 맥락 조회
	ctx := mem.Recall(req.SessionID)
	fmt.Printf("  ① 맥락조회: %d턴 %v\n", len(ctx), ctx)

	// ② 대상 판단
	t := classifyTarget(req.Utterance, ctx)
	fmt.Printf("  ② 대상: layer=%s lang=%s kind=%s forbidden=%v\n", t.Layer, t.Lang, t.Kind, t.Forbidden)

	// ③ code 생성·수정
	art, err := generateCode(t, req, ctx)
	if err != nil {
		fmt.Printf("  ③ 생성 차단: %s\n  ▶ OUTCOME: REJECTED_HARDRAIL\n\n", err.Error())
		return
	}
	fmt.Printf("  ③ 생성물: op=%s path=%s\n     diff=%s\n", art.Op, art.Path, art.Diff)

	// ④ 파이프라인 경유
	fmt.Println("  ④ 파이프라인:")
	for _, s := range toPipeline(art) {
		fmt.Printf("       - %s\n", s)
	}
	mem.Append(req.SessionID, req.Utterance) // 맥락 갱신
	fmt.Printf("  ▶ OUTCOME: QUEUED_FOR_DEPLOY (즉시집행 X)\n\n")
}

func main() {
	mem := &ConversationMemory{store: map[string][]string{}}

	fmt.Print("=== 개발엔진 실전 요청 처리 (맥락 흐름 포함) ===\n\n")

	reqs := []DevRequest{
		{"d1", "u1", "운영엔진 라우팅 로직을 더 빠르게 고쳐줘", "s1"},
		{"d1b", "u1", "방금 그 라우팅에 캐시도 붙여줘", "s1"}, // 같은 세션 = 맥락 이어짐
		{"d2", "u2", "전단 배치커밋 큐 성능 개선", "s2"},
		{"d3", "u3", "임베딩 OSS 모델을 우리 도메인에 맞게 개조", "s3"},
		{"d4", "u4", "쿼리 최적화 스키마 수정", "s4"},
		{"d5", "u5", "인프라 노드 직접 늘려줘", "s5"}, // ⛔ 개발엔진 금지 대상
	}
	for _, r := range reqs {
		handleDevRequest(r, mem)
	}

	fmt.Println("=== 맥락 흐름 검증 (s1 세션) ===")
	fmt.Printf("  s1 누적 맥락: %v\n", mem.Recall("s1"))
	fmt.Println("  ▶ 같은 세션의 연속 요청이 이전 턴을 이어받음 = 맥락 흐름 확인")

	fmt.Println("\n=== 종합 ===")
	fmt.Println("  ✓ 맥락조회: 세션별 이전 대화 이어받음")
	fmt.Println("  ✓ 레이어별 언어: go/rust/sql/python 대상별 분기")
	fmt.Println("  ✓ OSS 개조: patch-oss 경로")
	fmt.Println("  ✓ 생성물 CRUD: create/update op")
	fmt.Println("  ✓ 인프라 직접변경: 하드레일 차단 (개발↛인프라)")
	fmt.Println("  ✓ 모든 산출: 파이프라인 경유 (즉시집행 X)")
	fmt.Println("\n개발엔진 실전 흐름 검증 완료 — 장기 엔진 개발의 요청처리 골격")
}
