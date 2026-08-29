// file: internal/api/server.go
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/AnishPrakash/arena/internal/config"
	"github.com/AnishPrakash/arena/internal/langs"
	"github.com/AnishPrakash/arena/internal/obs"
	"github.com/AnishPrakash/arena/internal/ports"
)

type Server struct {
	cfg   config.Config
	log   *slog.Logger
	store ports.Store
	queue ports.Queue
	blobs ports.BlobStore
	board ports.Leaderboard
	bus   ports.EventBus
	rl    ports.RateLimiter
	langs *langs.Registry
}

type Deps struct {
	Cfg   config.Config
	Log   *slog.Logger
	Store ports.Store
	Queue ports.Queue
	Blobs ports.BlobStore
	Board ports.Leaderboard
	Bus   ports.EventBus
	RL    ports.RateLimiter
	Langs *langs.Registry
}

func NewServer(d Deps) *Server {
	return &Server{cfg: d.Cfg, log: d.Log, store: d.Store, queue: d.Queue,
		blobs: d.Blobs, board: d.Board, bus: d.Bus, rl: d.RL, langs: d.Langs}
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	// NOT middleware.RealIP: it rewrites r.RemoteAddr from X-Forwarded-For / X-Real-IP
	// whether or not our infrastructure actually sets those headers, so any client can
	// spoof its own address (GHSA-3fxj-6jh8-hvhx). Arena rate-limits per authenticated
	// user, never per IP, so there is nothing to gain from trusting them.
	r.Use(middleware.Recoverer)
	r.Use(s.metrics)
	// A hard request timeout is a resilience control, not a nicety: without it a slow
	// Postgres query holds a connection AND a goroutine indefinitely, and the API degrades
	// long before the database does.
	r.Use(middleware.Timeout(15 * time.Second))
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins: []string{"*"},
		AllowedMethods: []string{"GET", "POST", "PATCH", "OPTIONS"},
		AllowedHeaders: []string{"Authorization", "Content-Type", "Idempotency-Key"},
		MaxAge:         300,
	}))
	// Cap request bodies globally. Source files are kilobytes; a 10 MB "source file" is an
	// attack, and rejecting it at the edge costs nothing.
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req.Body = http.MaxBytesReader(w, req.Body, 1<<20) // 1 MiB
			next.ServeHTTP(w, req)
		})
	})

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	r.Get("/readyz", s.handleReady)
	// A judge opening the bare host should land somewhere meaningful rather than a 404.
	r.Get("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"service": "arena",
			"docs":    "https://github.com/AnishPrakash/arena#readme",
			"links": map[string]string{
				"health":      "/readyz",
				"problems":    "/v1/contests/technovit-speed/problems",
				"leaderboard": "/v1/contests/technovit-speed/leaderboard",
				"metrics":     "/metrics",
			},
		})
	})
	r.Handle("/metrics", promhttp.Handler())

	r.Route("/v1", func(r chi.Router) {
		r.Post("/auth/register", s.handleRegister)
		r.Post("/auth/login", s.handleLogin)
		r.Get("/languages", s.handleLanguages)

		r.Get("/contests/{slug}", s.handleContest)
		r.Get("/contests/{slug}/problems", s.handleProblems)
		r.Get("/contests/{slug}/leaderboard", s.handleLeaderboard)
		r.Get("/problems/{id}", s.handleProblem)

		r.Group(func(r chi.Router) {
			r.Use(s.requireAuth)
			r.Post("/problems/{id}/submissions", s.handleSubmit)
			r.Get("/submissions/{id}", s.handleGetSubmission)
			r.Get("/submissions", s.handleListSubmissions)
			r.Get("/submissions/{id}/events", s.handleSubmissionSSE)
		})

		r.Group(func(r chi.Router) {
			r.Use(s.requireAuth, s.requireRole("admin"))
			r.Post("/admin/contests/{slug}/rebuild-leaderboard", s.handleRebuild)
			r.Get("/admin/queue", s.handleQueueStats)
		})
	})

	// Runner-facing. Deliberately outside /v1 and behind a different credential so it can
	// be firewalled to the runner subnet at the ingress.
	r.Route("/internal", func(r chi.Router) {
		r.Use(s.requireRunnerToken)
		r.Patch("/submissions/{id}/judging", s.handleMarkJudging)
		r.Post("/results", s.handleResult)
	})

	return r
}

func (s *Server) metrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		route := chi.RouteContext(r.Context()).RoutePattern()
		if route == "" {
			route = "unmatched"
		}
		obs.HTTPDuration.WithLabelValues(route, r.Method,
			http.StatusText(ww.Status())).Observe(time.Since(start).Seconds())
	})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	if _, _, err := s.queue.Depth(ctx); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "queue unavailable")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ready"})
}

// ---------------------------------------------------------------- helpers

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

type errBody struct {
	Error string `json:"error"`
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, errBody{Error: msg})
}
