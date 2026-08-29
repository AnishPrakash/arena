// file: cmd/seed/main.go
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/AnishPrakash/arena/internal/adapters/blob"
	"github.com/AnishPrakash/arena/internal/adapters/postgres"
)

type tc struct {
	in, out string
	sample  bool
}

type prob struct {
	slug, title, statement string
	cpuMs, memMB           int
	checker                string
	tests                  []tc
}

func main() {
	ctx := context.Background()
	dsn := env("ARENA_PG_DSN", "postgres://arena:arena@localhost:5432/arena?sslmode=disable")
	blobRoot := env("ARENA_BLOB_ROOT", "/var/tmp/arena-blobs")

	st, err := postgres.New(ctx, dsn, 5)
	must(err)
	defer st.Close()
	must(st.Migrate(ctx))

	bs, err := blob.NewLocal(blobRoot)
	must(err)

	pool := st.Pool()

	// -------- users --------
	for _, u := range []struct{ handle, role string }{
		{"admin", "admin"}, {"setter", "setter"},
		{"anish", "participant"}, {"alice", "participant"}, {"bob", "participant"},
	} {
		hash, _ := bcrypt.GenerateFromPassword([]byte("password123"), bcrypt.DefaultCost)
		_, err := pool.Exec(ctx, `
			INSERT INTO users (handle, email, password_hash, role)
			VALUES ($1,$2,$3,$4) ON CONFLICT (handle) DO NOTHING`,
			u.handle, u.handle+"@arena.local", string(hash), u.role)
		must(err)
	}

	// -------- contest --------
	var contestID string
	must(pool.QueryRow(ctx, `
		INSERT INTO contests (slug, title, starts_at, ends_at, scoring_mode)
		VALUES ('technovit-speed','TechnoVIT Speed Coding', $1, $2, 'speed')
		ON CONFLICT (slug) DO UPDATE SET title = EXCLUDED.title
		RETURNING id`,
		time.Now().Add(-time.Hour), time.Now().Add(6*time.Hour),
	).Scan(&contestID))

	// -------- problems --------
	problems := []prob{
		{
			slug: "a-plus-b", title: "A + B", cpuMs: 1000, memMB: 128, checker: "token",
			statement: "Read two integers a and b, print a+b.",
			tests: []tc{
				{"1 2\n", "3\n", true},
				{"100 250\n", "350\n", false},
				{"-5 5\n", "0\n", false},
				{"1000000000 1000000000\n", "2000000000\n", false},
			},
		},
		{
			// This one exists to make the TLE demo meaningful: an O(n^2) solution fails,
			// an O(n) or O(n log n) one passes. That is "efficiency is matched".
			slug: "max-subarray", title: "Maximum Subarray Sum", cpuMs: 1000, memMB: 256,
			checker: "token",
			statement: "Given n and n integers, print the maximum sum of a contiguous " +
				"non-empty subarray. n can be up to 200000, so an O(n^2) solution will TLE.",
			tests: []tc{
				{"5\n-2 1 -3 4 -1\n", "4\n", true},
				{"3\n-5 -2 -9\n", "-2\n", false},
				{bigInput(200000), bigExpected(), false},
			},
		},
		{
			slug: "float-avg", title: "Average", cpuMs: 1000, memMB: 128, checker: "float",
			statement: "Print the mean of n numbers with absolute error <= 1e-6.",
			tests: []tc{
				{"3\n1 2 4\n", "2.3333333333\n", true},
				{"2\n1 2\n", "1.5\n", false},
			},
		},
	}

	for _, p := range problems {
		// testdata_version is the content hash of every input and output concatenated.
		// Any change to any test case changes it, which invalidates the verdict cache and
		// keeps historical verdicts explainable.
		h := sha256.New()
		for _, t := range p.tests {
			h.Write([]byte(t.in))
			h.Write([]byte(t.out))
		}
		version := hex.EncodeToString(h.Sum(nil))[:16]

		var pid string
		must(pool.QueryRow(ctx, `
			INSERT INTO problems (contest_id, slug, title, statement_md,
			                      cpu_ms, wall_ms, mem_mb, checker_type, testdata_version)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			ON CONFLICT (contest_id, slug) DO UPDATE SET
			  title=EXCLUDED.title, statement_md=EXCLUDED.statement_md,
			  cpu_ms=EXCLUDED.cpu_ms, wall_ms=EXCLUDED.wall_ms, mem_mb=EXCLUDED.mem_mb,
			  checker_type=EXCLUDED.checker_type, testdata_version=EXCLUDED.testdata_version
			RETURNING id`,
			contestID, p.slug, p.title, p.statement,
			p.cpuMs, p.cpuMs*3, p.memMB, p.checker, version,
		).Scan(&pid))

		_, err = pool.Exec(ctx, `DELETE FROM test_cases WHERE problem_id = $1`, pid)
		must(err)

		for i, t := range p.tests {
			inKey := fmt.Sprintf("testdata/%s/%s/%d.in", version, p.slug, i)
			outKey := fmt.Sprintf("testdata/%s/%s/%d.out", version, p.slug, i)
			must(bs.Put(ctx, inKey, strings.NewReader(t.in)))
			must(bs.Put(ctx, outKey, strings.NewReader(t.out)))
			_, err = pool.Exec(ctx, `
				INSERT INTO test_cases (problem_id, idx, input_ref, output_ref, is_sample, points)
				VALUES ($1,$2,$3,$4,$5,$6)`,
				pid, i, inKey, outKey, t.sample, 100/len(p.tests))
			must(err)
		}
		fmt.Printf("seeded problem %-16s testdata_version=%s tests=%d\n", p.slug, version, len(p.tests))
	}

	fmt.Println("\nseed complete.")
	fmt.Println("  contest : technovit-speed")
	fmt.Println("  login   : anish / password123")
}

func bigInput(n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d\n", n)
	for i := 0; i < n; i++ {
		v := (i*7919)%2001 - 1000 // deterministic, no RNG: the seed must be reproducible
		fmt.Fprintf(&b, "%d ", v)
	}
	b.WriteString("\n")
	return b.String()
}

func bigExpected() string {
	n := 200000
	best, cur := int64(-1<<62), int64(0)
	for i := 0; i < n; i++ {
		v := int64((i*7919)%2001 - 1000)
		if cur < 0 {
			cur = 0
		}
		cur += v
		if cur > best {
			best = cur
		}
	}
	return fmt.Sprintf("%d\n", best)
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
