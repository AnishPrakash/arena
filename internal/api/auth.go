// file: internal/api/auth.go
package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type ctxKey string

const ctxUser ctxKey = "arena.user"

type Claims struct {
	UserID string `json:"uid"`
	Handle string `json:"h"`
	Role   string `json:"r"`
	jwt.RegisteredClaims
}

func (s *Server) issueToken(userID, handle, role string) (string, error) {
	now := time.Now()
	c := Claims{
		UserID: userID, Handle: handle, Role: role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(s.cfg.JWTTTL)),
			Issuer:    "arena",
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte(s.cfg.JWTSecret))
}

func (s *Server) parseToken(tok string) (*Claims, error) {
	var c Claims
	_, err := jwt.ParseWithClaims(tok, &c, func(t *jwt.Token) (any, error) {
		// Pinning the algorithm is not optional. Accepting whatever the token declares is
		// the "alg: none" / HS-vs-RS confusion class of vulnerability.
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return []byte(s.cfg.JWTSecret), nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer("arena"))
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// requireAuth is the authentication middleware.
//
// Note on CSRF: Arena authenticates with a Bearer token in the Authorization header, never
// with a cookie. A browser does not attach Authorization headers to cross-site requests
// automatically, so the CSRF attack class does not apply. That is a design choice, not an
// omission - if you ever move to cookie auth you must add SameSite=Lax plus a double-submit
// token. Say exactly this in the README; it shows the threat was considered.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, "Bearer ") {
			writeErr(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		c, err := s.parseToken(strings.TrimPrefix(h, "Bearer "))
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "invalid or expired token")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxUser, c)))
	})
}

func (s *Server) requireRole(roles ...string) func(http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, r := range roles {
		allowed[r] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c := userFrom(r.Context())
			if c == nil || !allowed[c.Role] {
				writeErr(w, http.StatusForbidden, "insufficient role")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// requireRunnerToken guards the internal result endpoint.
//
// Runner nodes hold a shared secret and NOTHING else - no database credentials, no JWT
// signing key. If a participant escapes a sandbox, the most they can reach is the ability
// to post results, which is bounded and auditable. In production this becomes a per-runner
// mTLS identity; the shared secret is the documented 2-day simplification.
func (s *Server) requireRunnerToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtleCompare(r.Header.Get("X-Runner-Token"), s.cfg.RunnerToken) {
			next.ServeHTTP(w, r)
			return
		}
		writeErr(w, http.StatusUnauthorized, "bad runner token")
	})
}

// subtleCompare is constant time, so token comparison does not leak length or prefix.
func subtleCompare(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

func userFrom(ctx context.Context) *Claims {
	c, _ := ctx.Value(ctxUser).(*Claims)
	return c
}
