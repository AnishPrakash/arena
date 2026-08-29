// file: testdata/golden/golden_test.go
//go:build golden

package golden

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

type Case struct {
	Name            string   `yaml:"name"`
	File            string   `yaml:"file"`
	Lang            string   `yaml:"lang"`
	Expect          string   `yaml:"expect"`
	WantVerdict     string   `yaml:"want_verdict"`
	ExpectAny       []string `yaml:"expect_any"`
	MustNotBe       string   `yaml:"must_not_be"`
	ProblemOverride string   `yaml:"problem_override"`
	Why             string   `yaml:"why"`
}

type Suite struct {
	Problem string `yaml:"problem"`
	Cases   []Case `yaml:"cases"`
}

func api() string {
	if v := os.Getenv("ARENA_API_BASE"); v != "" {
		return v
	}
	return "http://localhost:8080"
}

func TestGolden(t *testing.T) {
	b, err := os.ReadFile("cases.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var suite Suite
	if err := yaml.Unmarshal(b, &suite); err != nil {
		t.Fatal(err)
	}

	token := login(t)
	problems := listProblems(t)

	for _, c := range suite.Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			// Each case runs independently: one broken sandbox behaviour should show up as
			// one red test, not cascade through the suite.
			t.Parallel()

			slug := suite.Problem
			if c.ProblemOverride != "" {
				slug = c.ProblemOverride
			}
			pid, ok := problems[slug]
			if !ok {
				t.Fatalf("problem %q not seeded", slug)
			}

			src, err := os.ReadFile(filepath.Join("src", c.File))
			if err != nil {
				t.Fatal(err)
			}

			id := submit(t, token, pid, c.Lang, string(src))
			got, cpu, mem := await(t, token, id, 240*time.Second)

			want := c.Expect
			if c.WantVerdict != "" {
				want = c.WantVerdict
			}

			switch {
			case len(c.ExpectAny) > 0:
				for _, w := range c.ExpectAny {
					if got == w {
						t.Logf("OK %s (cpu=%dms mem=%dKB) — %s", got, cpu, mem, c.Why)
						return
					}
				}
				t.Fatalf("got %s, want one of %v — %s", got, c.ExpectAny, c.Why)
			case want == "AC_NOT":
				if got == "AC" {
					t.Fatalf("must not be AC — %s", c.Why)
				}
			default:
				if got != want {
					t.Fatalf("got %s, want %s (cpu=%dms mem=%dKB) — %s", got, want, cpu, mem, c.Why)
				}
			}
			if c.MustNotBe != "" && got == c.MustNotBe {
				t.Fatalf("verdict %s is forbidden for this case — %s", got, c.Why)
			}
			t.Logf("OK %s (cpu=%dms mem=%dKB)", got, cpu, mem)
		})
	}
}

// ---------------------------------------------------------------- helpers

func login(t *testing.T) string {
	t.Helper()
	body := `{"handle":"anish","password":"password123"}`
	resp, err := http.Post(api()+"/v1/auth/login", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("api unreachable at %s: %v (is the stack up?)", api(), err)
	}
	defer resp.Body.Close()
	var out struct {
		Token string `json:"token"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Token == "" {
		t.Fatal("login failed — run `make seed`")
	}
	return out.Token
}

func listProblems(t *testing.T) map[string]string {
	t.Helper()
	resp, err := http.Get(api() + "/v1/contests/technovit-speed/problems")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var ps []struct{ ID, Slug string }
	json.NewDecoder(resp.Body).Decode(&ps)
	m := map[string]string{}
	for _, p := range ps {
		m[p.Slug] = p.ID
	}
	return m
}

func submit(t *testing.T, token, problem, lang, src string) string {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{"language": lang, "source": src})
	req, _ := http.NewRequest("POST",
		fmt.Sprintf("%s/v1/problems/%s/submissions", api(), problem), bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", fmt.Sprintf("golden-%d", time.Now().UnixNano()))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out struct{ ID string }
	json.Unmarshal(raw, &out)
	if out.ID == "" {
		t.Fatalf("submit failed: %s", raw)
	}
	return out.ID
}

func await(t *testing.T, token, id string, timeout time.Duration) (verdict string, cpu, mem int64) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest("GET", api()+"/v1/submissions/"+id, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		var out struct {
			Status  string `json:"status"`
			Verdict string `json:"verdict"`
			CPUms   int64  `json:"cpu_ms"`
			MemKB   int64  `json:"mem_kb"`
		}
		json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if out.Status == "DONE" || out.Status == "FAILED" {
			return out.Verdict, out.CPUms, out.MemKB
		}
		time.Sleep(700 * time.Millisecond)
	}
	t.Fatalf("submission %s never finished within %s", id, timeout)
	return
}
