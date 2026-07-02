package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────
// HTTP 표면 — REST /v1. 모든 변경 호출: JWT 검증 → org_id 주입(RLS) → 멱등성.
// (stdlib net/http 만 사용 — 외부 의존 0)
// ─────────────────────────────────────────────────────────────────────────

type Server struct{ app *App }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// 인증 미들웨어: Bearer 토큰 → Claims → context. org_id 주입(=RLS 경계).
type ctxKey string

const claimsKey ctxKey = "claims"

func (s *Server) auth(next func(http.ResponseWriter, *http.Request, Claims)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		c, err := s.app.auth.VerifyToken(r.Context(), tok)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r, c)
	}
}

func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()

	// 공개: 온보딩 + OIDC 디스커버리(IdP 발급)
	mux.HandleFunc("POST /v1/auth/signup", s.handleSignup)
	mux.HandleFunc("GET /v1/.well-known/jwks.json", s.handleJWKS)

	// 인증 필요: P0 루프
	mux.HandleFunc("POST /v1/projects", s.auth(s.handleCreateProject))
	mux.HandleFunc("POST /v1/projects/{pid}/branches", s.auth(s.handleCreateBranch))
	mux.HandleFunc("POST /v1/branches/{bid}/endpoints", s.auth(s.handleStartEndpoint))
	mux.HandleFunc("GET /v1/endpoints/{eid}", s.auth(s.handleGetEndpoint))
	mux.HandleFunc("POST /v1/endpoints/{eid}/suspend", s.auth(s.handleSuspend))
	mux.HandleFunc("GET /v1/operations/{oid}", s.auth(s.handleGetOperation))
	mux.HandleFunc("GET /v1/usage", s.auth(s.handleUsage))

	return mux
}

func (s *Server) handleSignup(w http.ResponseWriter, r *http.Request) {
	var body struct{ Name, OwnerUserID string }
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Name == "" || body.OwnerUserID == "" {
		writeJSON(w, 400, map[string]string{"error": "name and owner_user_id required"})
		return
	}
	org, token, err := s.app.Signup(r.Context(), body.Name, body.OwnerUserID)
	if err != nil { writeJSON(w, 500, map[string]string{"error": err.Error()}); return }
	writeJSON(w, 201, map[string]any{"org": org, "token": token})
}

func (s *Server) handleJWKS(w http.ResponseWriter, r *http.Request) {
	j, _ := s.app.auth.JWKS(r.Context())
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(j))
}

// 멱등성 체크 → 비동기 op 시작 → 202 + operation_id.
func (s *Server) startOp(w http.ResponseWriter, r *http.Request, c Claims, make func(context.Context) *Operation) {
	key := r.Header.Get("Idempotency-Key")
	if prev, ok := s.app.store.idempLookup(key); ok {
		writeJSON(w, 202, map[string]string{"operation_id": prev, "idempotent_replay": "true"})
		return
	}
	op := make(context.WithValue(r.Context(), claimsKey, c))
	s.app.store.idempStore(key, op.ID)
	w.Header().Set("Location", "/v1/operations/"+op.ID)
	writeJSON(w, 202, map[string]string{"operation_id": op.ID})
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request, c Claims) {
	var body struct{ Name string }
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.startOp(w, r, c, func(ctx context.Context) *Operation {
		return s.app.CreateProject(ctx, c.OrgID, body.Name)
	})
}

func (s *Server) handleCreateBranch(w http.ResponseWriter, r *http.Request, c Claims) {
	pid := r.PathValue("pid")
	var body struct{ ParentBranchID string }
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.startOp(w, r, c, func(ctx context.Context) *Operation {
		return s.app.CreateBranch(ctx, c.OrgID, pid, body.ParentBranchID)
	})
}

func (s *Server) handleStartEndpoint(w http.ResponseWriter, r *http.Request, c Claims) {
	bid := r.PathValue("bid")
	var body struct{ MinCU, MaxCU float64 }
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.MaxCU == 0 { body.MinCU, body.MaxCU = 0.25, 2 }
	s.startOp(w, r, c, func(ctx context.Context) *Operation {
		return s.app.StartEndpoint(ctx, c.OrgID, bid, body.MinCU, body.MaxCU)
	})
}

func (s *Server) handleGetEndpoint(w http.ResponseWriter, r *http.Request, c Claims) {
	ep, ok := s.app.store.getEndpoint(c.OrgID, r.PathValue("eid"))
	if !ok { writeJSON(w, 404, map[string]string{"error": "not found"}); return }
	writeJSON(w, 200, ep)
}

func (s *Server) handleSuspend(w http.ResponseWriter, r *http.Request, c Claims) {
	if err := s.app.SuspendEndpoint(r.Context(), c.OrgID, r.PathValue("eid")); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()}); return
	}
	writeJSON(w, 200, map[string]string{"state": EndpointSuspended})
}

func (s *Server) handleGetOperation(w http.ResponseWriter, r *http.Request, c Claims) {
	op, ok := s.app.store.opSnapshot(c.OrgID, r.PathValue("oid"))
	if !ok { writeJSON(w, 404, map[string]string{"error": "not found"}); return }
	writeJSON(w, 200, op)
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request, c Claims) {
	writeJSON(w, 200, s.app.Usage(c.OrgID))
}
