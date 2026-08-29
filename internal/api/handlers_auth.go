// file: internal/api/handlers_auth.go
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"

	"golang.org/x/crypto/bcrypt"

	"github.com/AnishPrakash/arena/internal/adapters/postgres"
	"github.com/AnishPrakash/arena/internal/ports"
	"github.com/go-chi/chi/v5"
)

var handleRe = regexp.MustCompile(`^[a-zA-Z0-9_]{3,24}$`)

type credReq struct {
	Handle   string `json:"handle"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var in credReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if !handleRe.MatchString(in.Handle) {
		writeErr(w, 400, "handle must be 3-24 chars of [a-zA-Z0-9_]")
		return
	}
	if len(in.Password) < 8 {
		writeErr(w, 400, "password must be at least 8 characters")
		return
	}
	// bcrypt, not sha256: it is deliberately slow and salted, so a leaked table is not a
	// leaked password list.
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		writeErr(w, 500, "hash failure")
		return
	}
	u, err := s.store.CreateUser(r.Context(), ports.User{
		Handle: in.Handle, Email: in.Email,
		PasswordHash: string(hash), Role: "participant",
	})
	if err != nil {
		writeErr(w, 409, "handle already taken")
		return
	}
	tok, _ := s.issueToken(u.ID, u.Handle, u.Role)
	writeJSON(w, 201, map[string]any{"token": tok, "user": u})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in credReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	u, err := s.store.GetUserByHandle(r.Context(), in.Handle)
	if err != nil {
		// Same message and same code for "no such user" and "wrong password", so the
		// endpoint cannot be used to enumerate registered handles.
		writeErr(w, 401, "invalid credentials")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(in.Password)) != nil {
		writeErr(w, 401, "invalid credentials")
		return
	}
	tok, err := s.issueToken(u.ID, u.Handle, u.Role)
	if err != nil {
		writeErr(w, 500, "token error")
		return
	}
	writeJSON(w, 200, map[string]any{"token": tok, "user": u})
}

func (s *Server) handleLanguages(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, s.langs.List())
}

func (s *Server) handleContest(w http.ResponseWriter, r *http.Request) {
	c, err := s.store.GetContestBySlug(r.Context(), chi.URLParam(r, "slug"))
	if errors.Is(err, postgres.ErrNotFound) {
		writeErr(w, 404, "contest not found")
		return
	}
	if err != nil {
		writeErr(w, 500, "lookup failed")
		return
	}
	writeJSON(w, 200, c)
}

func (s *Server) handleProblems(w http.ResponseWriter, r *http.Request) {
	c, err := s.store.GetContestBySlug(r.Context(), chi.URLParam(r, "slug"))
	if err != nil {
		writeErr(w, 404, "contest not found")
		return
	}
	ps, err := s.store.ListProblems(r.Context(), c.ID)
	if err != nil {
		writeErr(w, 500, "lookup failed")
		return
	}
	writeJSON(w, 200, ps)
}

func (s *Server) handleProblem(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.GetProblem(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, 404, "problem not found")
		return
	}
	writeJSON(w, 200, p)
}

func (s *Server) handleLeaderboard(w http.ResponseWriter, r *http.Request) {
	c, err := s.store.GetContestBySlug(r.Context(), chi.URLParam(r, "slug"))
	if err != nil {
		writeErr(w, 404, "contest not found")
		return
	}
	top, err := s.board.Top(r.Context(), c.ID, 100)
	if err != nil {
		writeErr(w, 500, "leaderboard unavailable")
		return
	}
	writeJSON(w, 200, map[string]any{"contest": c.Slug, "mode": c.ScoringMode, "entries": top})
}

func (s *Server) handleRebuild(w http.ResponseWriter, r *http.Request) {
	c, err := s.store.GetContestBySlug(r.Context(), chi.URLParam(r, "slug"))
	if err != nil {
		writeErr(w, 404, "contest not found")
		return
	}
	stats, err := s.store.ContestStats(r.Context(), c.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := s.board.Rebuild(r.Context(), c.ID, stats, c.ScoringMode, c.StartsAt); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"rebuilt": len(stats)})
}

func (s *Server) handleQueueStats(w http.ResponseWriter, r *http.Request) {
	pending, backlog, err := s.queue.Depth(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"in_flight": pending, "waiting": backlog})
}
