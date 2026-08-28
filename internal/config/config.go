// file: internal/config/config.go
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config is loaded once at boot and never mutated. Every knob is an environment variable
// with a documented default, which is what makes the system reproducible across laptop,
// compose, CI and cloud without code changes.
type Config struct {
	Env      string
	HTTPAddr string
	LogLevel string

	PGDSN     string
	PGMaxConn int32

	RedisAddr string
	RedisDB   int

	JWTSecret   string
	JWTTTL      time.Duration
	RunnerToken string

	Stream      string
	Group       string
	LeaseTTL    time.Duration
	MaxAttempts int

	BlobDriver string
	BlobRoot   string

	RLSubmitPerMin int
	RLBurst        int

	RunnerID      string
	RunnerSlots   int
	APIBase       string
	SandboxDriver string
	BoxRoot       string
	CPUSetBase    int
}

func Load() (Config, error) {
	c := Config{
		Env:      str("ARENA_ENV", "dev"),
		HTTPAddr: str("ARENA_HTTP_ADDR", ":8080"),
		LogLevel: str("ARENA_LOG_LEVEL", "info"),

		PGDSN:     str("ARENA_PG_DSN", "postgres://arena:arena@localhost:5432/arena?sslmode=disable"),
		PGMaxConn: int32(num("ARENA_PG_MAX_CONNS", 20)),

		RedisAddr: str("ARENA_REDIS_ADDR", "localhost:6379"),
		RedisDB:   num("ARENA_REDIS_DB", 0),

		JWTSecret:   str("ARENA_JWT_SECRET", ""),
		JWTTTL:      dur("ARENA_JWT_TTL", 24*time.Hour),
		RunnerToken: str("ARENA_RUNNER_TOKEN", ""),

		Stream:      str("ARENA_STREAM", "judge.jobs"),
		Group:       str("ARENA_GROUP", "judges"),
		LeaseTTL:    dur("ARENA_LEASE_TTL", 90*time.Second),
		MaxAttempts: num("ARENA_MAX_ATTEMPTS", 3),

		BlobDriver: str("ARENA_BLOB_DRIVER", "local"),
		BlobRoot:   str("ARENA_BLOB_ROOT", "/var/tmp/arena-blobs"),

		RLSubmitPerMin: num("ARENA_RL_SUBMIT_PER_MIN", 12),
		RLBurst:        num("ARENA_RL_BURST", 4),

		RunnerID:      str("ARENA_RUNNER_ID", "runner-local-1"),
		RunnerSlots:   num("ARENA_RUNNER_SLOTS", 2),
		APIBase:       str("ARENA_API_BASE", "http://localhost:8080"),
		SandboxDriver: str("ARENA_SANDBOX", "docker"),
		BoxRoot:       str("ARENA_BOX_ROOT", "/var/tmp/arena-boxes"),
		CPUSetBase:    num("ARENA_CPUSET_BASE", 1),
	}

	// Fail fast on missing secrets in production. A judge that silently boots with an
	// empty JWT secret is worse than one that refuses to start.
	if c.Env == "prod" {
		if len(c.JWTSecret) < 32 {
			return c, fmt.Errorf("ARENA_JWT_SECRET must be at least 32 bytes in prod")
		}
		if len(c.RunnerToken) < 16 {
			return c, fmt.Errorf("ARENA_RUNNER_TOKEN must be at least 16 bytes in prod")
		}
	}
	if c.JWTSecret == "" {
		c.JWTSecret = "dev-only-insecure-secret-do-not-use-in-production"
	}
	if c.RunnerToken == "" {
		c.RunnerToken = "dev-runner-token"
	}
	return c, nil
}

func str(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func num(k string, d int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return d
}

func dur(k string, d time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if p, err := time.ParseDuration(v); err == nil {
			return p
		}
	}
	return d
}
