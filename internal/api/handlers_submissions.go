// file: internal/api/handlers_submissions.go
package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/AnishPrakash/arena/internal/core"
	"github.com/AnishPrakash/arena/internal/obs"
	"github.com/AnishPrakash/arena/internal/ports"
)

type submitReq struct {
	Language string `json:"language"`
	Source   string `json:"source"`
}

const maxSourceBytes = 256 * 1024 // 256 KiB: generous for any solution, hostile to abuse

// handleSubmit is the hot path. Its contract is: bounded work, no execution, always fast.
//
// Order of operations is chosen so the cheapest rejection happens first:
//
//	validate -> rate limit -> idempotency -> catalogue -> store blob -> insert -> enqueue.
//
// Anything that touches Redis or Postgres comes after the checks that need neither.
func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := userFrom(ctx)

	var in submitReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if strings.TrimSpace(in.Source) == "" {
		writeErr(w, 400, "empty source")
		return
	}
	if len(in.Source) > maxSourceBytes {
		writeErr(w, 413, fmt.Sprintf("source exceeds %d bytes", maxSourceBytes))
		return
	}
	manifest, ok := s.langs.Get(in.Language)
	if !ok {
		writeErr(w, 400, "unsupported language: "+in.Language)
		return
	}

	// ---- rate limit: cost control first, abuse control second ----
	allowed, retry, _ := s.rl.Allow(ctx, "submit:"+u.UserID, s.cfg.RLSubmitPerMin, s.cfg.RLBurst)
	if !allowed {
		obs.RateLimited.Inc()
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		writeErr(w, http.StatusTooManyRequests, "too many submissions; slow down")
		return
	}

	// ---- idempotency: a retried request must never create a second submission ----
	// Clients retry on timeouts and flaky campus wifi. Without this, one participant's
	// double-tap becomes two judged submissions and a wrong penalty count.
	idem := r.Header.Get("Idempotency-Key")
	if idem != "" {
		if existing, found, _ := s.store.FindByIdempotencyKey(ctx, u.UserID, idem); found {
			writeJSON(w, 200, existing)
			return
		}
	}

	problem, err := s.store.GetProblem(ctx, chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, 404, "problem not found")
		return
	}
	tests, err := s.store.ListTestCases(ctx, problem.ID)
	if err != nil || len(tests) == 0 {
		writeErr(w, 503, "problem has no test data")
		return
	}

	// ---- content-addressed source storage ----
	// Normalising line endings before hashing means a Windows and a Linux submission of
	// identical code share a cache entry instead of judging twice.
	normalized := strings.ReplaceAll(in.Source, "\r\n", "\n")
	sum := sha256.Sum256([]byte(normalized))
	srcHash := hex.EncodeToString(sum[:])
	srcRef := "src/" + srcHash
	if exists, _ := s.blobs.Exists(ctx, srcRef); !exists {
		if err := s.blobs.Put(ctx, srcRef, strings.NewReader(normalized)); err != nil {
			writeErr(w, 500, "could not store source")
			return
		}
	}

	sub, err := s.store.CreateSubmission(ctx, ports.Submission{
		UserID: u.UserID, ProblemID: problem.ID, ContestID: problem.ContestID,
		Language: manifest.ID, SourceRef: srcRef, SourceHash: srcHash,
		TestdataVersion: problem.TestdataVersion, IdempotencyKey: idem,
	})
	if err != nil {
		// A unique-violation here means a concurrent request with the same key won the
		// race; return that one instead of erroring.
		if existing, found, _ := s.store.FindByIdempotencyKey(ctx, u.UserID, idem); found {
			writeJSON(w, 200, existing)
			return
		}
		writeErr(w, 500, "could not create submission")
		return
	}

	// ---- verdict cache: the cheapest capacity in the system ----
	// Participants resubmit identical code constantly. If we have already judged this
	// exact (source, language, image, testdata) tuple, we replay the stored result and
	// never touch a runner. Typically 15-30% of contest submissions.
	limits := core.LimitSet{
		Compile: manifest.EffectiveCompileLimits(),
		// Per-language scaling applied HERE, once, so the runner never has to know about
		// language fairness policy — and so the cache key reflects the real envelope.
		Run: manifest.ScaleRunLimits(problem.Limits),
	}
	cacheKey := VerdictCacheKey(srcHash, manifest.ID, manifest.Image, problem.TestdataVersion, limits)
	if cached, hit, _ := s.store.CachedVerdict(ctx, cacheKey); hit {
		obs.CacheHits.Inc()
		cached.SubmissionID = sub.ID
		cached.JudgedAt = time.Now()
		cached.RunnerID = "cache"
		if err := s.store.SaveResult(ctx, cached); err == nil {
			s.afterVerdict(ctx, sub, cached, problem)
			full, _ := s.store.GetSubmission(ctx, sub.ID)
			writeJSON(w, 200, full)
			return
		}
	}

	// ---- build the self-contained job ----
	job := core.JobSpec{
		SubmissionID:    sub.ID,
		Attempt:         1,
		ContestID:       problem.ContestID,
		ProblemID:       problem.ID,
		UserID:          u.UserID,
		Language:        manifest.ID,
		SourceRef:       srcRef,
		SourceHash:      srcHash,
		ImageDigest:     manifest.Image,
		TestdataVersion: problem.TestdataVersion,
		Tests:           tests,
		Limits:          limits,
		Checker:         problem.Checker,
		Policy:          core.DefaultPolicy(),
		EnqueuedAt:      time.Now(),
	}
	if err := s.queue.Publish(ctx, job); err != nil {
		// The row exists but no job does. The reconciler (4.9) re-enqueues it, so we
		// return 202 rather than failing the participant's submission.
		s.log.Error("enqueue failed", "submission", sub.ID, "err", err)
	}

	writeJSON(w, http.StatusAccepted, sub)
}

// VerdictCacheKey binds every input that can change a verdict. Change any one of them and
// the key changes, so the cache invalidates itself with no TTL and no manual purge.
//
// The LIMITS are part of the key, and that is not optional. Without them, raising a
// problem's time limit — or fixing a platform default, as happened when the compile
// step's RLIMIT_FSIZE was strangling the linker — leaves every previously cached verdict
// in place, and the corrected judge is never invoked for code it has seen before.
func VerdictCacheKey(sourceHash, lang, image, testdataVersion string, lim core.LimitSet) string {
	limJSON, _ := json.Marshal(lim)
	h := sha256.Sum256([]byte(sourceHash + "|" + lang + "|" + image + "|" +
		testdataVersion + "|" + string(limJSON)))
	return hex.EncodeToString(h[:])
}

func (s *Server) handleGetSubmission(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	sub, err := s.store.GetSubmission(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, 404, "submission not found")
		return
	}
	// Authorisation: your own submissions, or any if you are staff. Without this check a
	// participant can read every other participant's source and results by guessing IDs.
	if sub.UserID != u.UserID && u.Role != "admin" && u.Role != "setter" {
		writeErr(w, 403, "not your submission")
		return
	}
	writeJSON(w, 200, sub)
}

func (s *Server) handleListSubmissions(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	f := ports.SubmissionFilter{
		UserID:    u.UserID, // participants always see only their own
		ProblemID: r.URL.Query().Get("problem"),
		ContestID: r.URL.Query().Get("contest"),
	}
	if u.Role == "admin" && r.URL.Query().Get("user") != "" {
		f.UserID = r.URL.Query().Get("user")
	}
	f.Limit, _ = strconv.Atoi(r.URL.Query().Get("limit"))
	f.Offset, _ = strconv.Atoi(r.URL.Query().Get("offset"))
	out, err := s.store.ListSubmissions(r.Context(), f)
	if err != nil {
		writeErr(w, 500, "query failed")
		return
	}
	writeJSON(w, 200, out)
}
