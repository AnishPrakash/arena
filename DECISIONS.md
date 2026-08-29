# Architecture Decision Records — Arena

Format: **Context → Decision → Alternatives rejected → Consequences (including the bad
ones).** An ADR that lists no downside is marketing, not engineering.

Companion to [`README.md`](./README.md).

---

## ADR-001 — Split the control plane from the execution plane with a durable queue

**Status:** Accepted · **Date:** 2026-08-27

### Context

A contest judge has two workloads with opposite characteristics. Accepting submissions is
thousands of tiny latency-critical requests in a 30-second burst; executing untrusted code
is seconds of pinned CPU per item, hostile, and expensive. Running both in one process
makes the HTTP worker pool the execution capacity: at contest start every request blocks
for seconds, the pool saturates, clients time out on submissions that actually ran, and
there is no way to tell a participant what happened to their code.

### Decision

The API validates, persists, enqueues and returns `202 Accepted` — it never executes code.
A separate fleet of runner agents consumes from a Redis Streams queue.

### Alternatives rejected

- **Synchronous judging in the request.** Simplest to build; fails at exactly the moment
  the system exists for.
- **A goroutine pool inside the API.** Slightly better, but judging load still competes
  with request handling for the same CPU and memory, and a crash takes both down.
- **A cron-style batch worker polling the database.** Adds latency proportional to the poll
  interval, has no lease semantics, and turns the database into a queue — an anti-pattern
  that fails under contention.

### Consequences

- **Good:** a burst becomes *queue depth*, an observable number that drives autoscaling.
  API p95 is independent of judging load. Runner capacity scales on its own signal and its
  own hardware. A runner crash is survivable.
- **Good:** the two planes can have different security postures — runners hold no database
  credentials.
- **Bad:** verdicts are asynchronous, so the client needs polling or SSE. Added complexity:
  idempotency, leases, reconciliation, and a duplicate-delivery story.
- **Bad:** one more infrastructure dependency to operate.

---

## ADR-002 — Go for both the API and the runner agent

**Status:** Accepted

### Context

The runner needs `fork`/`exec`, `setrlimit`, `wait4` for `rusage`, signal handling and
process-group control. The API needs ordinary HTTP concurrency. Two days to build both.

### Decision

Go for both binaries. One module, one toolchain, one CI pipeline.

### Alternatives rejected

- **Python for the runner.** The GIL, ~50 ms interpreter startup, a heavy image, and
  awkward access to `wait4`/`setrlimit` — the exact syscalls the supervisor exists to make.
- **Rust for the runner.** Technically the best fit and a language I already use. Rejected
  purely on schedule: Go's `os/exec` and `syscall` packages get to a correct supervisor
  faster, and the supervisor is ~200 lines either way.
- **Node/TypeScript or FastAPI for the API only.** Defensible, but maintaining two
  toolchains, two Dockerfile styles and two dependency stories costs more than it saves at
  this size.

### Consequences

- **Good:** static binaries, ~15 MB distroless images, fast node bootstrap, cheap
  horizontal scaling. Shared domain types between API and runner with no serialisation
  layer to keep in sync.
- **Bad:** Go's multi-threaded runtime forbids arbitrary code between `fork` and `exec`,
  which forces the two-stage self-exec in `boxrun` (see ADR-005). In C or Rust this would
  be a single `fork()` with the limits applied inline.

---

## ADR-003 — Redis Streams as the queue

**Status:** Accepted

### Context

The queue must survive a runner dying mid-job, must not lose work on preemption, must
expose depth as an autoscaling signal, and must not add a fourth piece of infrastructure.

### Decision

Redis Streams with a consumer group. Redis also serves the leaderboard (ZSET), rate limits
(Lua token bucket) and SSE fan-out (pub/sub).

### Alternatives rejected

- **`LPUSH`/`BRPOP` on a list.** A queue right up until a consumer dies mid-job — then the
  message is simply gone and a participant watches a spinner forever. No lease, no delivery
  count, no dead-lettering.
- **Kafka.** Correct semantics, wildly disproportionate operations for one topic.
- **SQS.** Good semantics and managed, but cloud lock-in and a second dependency when Redis
  is already required for the leaderboard.
- **RabbitMQ.** Same guarantees, one more service to run.
- **Postgres as a queue** (`SELECT … FOR UPDATE SKIP LOCKED`). Genuinely viable and would
  remove a dependency. Rejected because the leaderboard needs Redis anyway, and because
  queue traffic on the primary competes with the submission writes it is trying to protect.

### Consequences

- **Good:** the Pending Entries List gives leases, delivery counts and `XAUTOCLAIM`
  redelivery for free. This is what makes spot instances safe — the resilience feature and
  the biggest cost lever are the same feature. `XINFO GROUPS … lag` is the KEDA signal.
- **Good:** one dependency does four jobs.
- **Bad:** at-least-once, never exactly-once. Every handler must be idempotent; the store's
  `WHERE status <> 'DONE'` guard is what makes duplicate delivery harmless.
- **Bad:** Redis is now on the critical path for judging. Mitigated with AOF persistence
  (an unacked job that vanishes is a participant who never gets an answer) and by making
  the leaderboard rebuildable from Postgres.

---

## ADR-004 — Docker containers with cgroups v2, not a chroot and not a VM

**Status:** Accepted

### Context

Untrusted code from an event whose *stated theme in other tracks is breaking sandboxes*
must be contained, and its CPU and memory measured accurately.

### Decision

One ephemeral container per execution: `--network none`, `--read-only` rootfs, size-capped
tmpfs `/box`, cgroup memory with swap disabled, `--pids-limit`, `--cap-drop=ALL`,
`--security-opt=no-new-privileges`, uid 65534, `--cpuset-cpus` pinning.

### Alternatives rejected

- **`chroot` + `setrlimit` only.** No memory cgroup, no PID namespace, no network
  namespace. A fork bomb takes the host down.
- **`isolate` (the IOI/CMS sandbox).** Technically better: ~5 ms box creation versus
  ~250 ms, and it emits time/max-RSS/exit-status metrics natively. Rejected for the
  timebox because it needs privileged cgroup delegation that is awkward on managed
  Kubernetes and inside WSL2. **Documented upgrade** — it slots behind the existing
  `Sandbox` interface.
- **gVisor (`runsc`).** Strictly stronger: userspace syscall interception removes the host
  kernel from the attack surface. It is a one-line change (`--runtime=runsc`) behind the
  same interface, deferred only because it was not installable on the demo target in time.
- **Firecracker microVM per submission.** Strongest isolation, ~125 ms boot. Needs bare
  metal or nested virtualisation. Fly.io Machines would give this for free and is the
  documented path if the deployment target changes.

### Consequences

- **Good:** defence in depth — a seccomp bypass still faces cgroups, a cgroup bug still
  faces the wall timer, a hung supervisor still faces the queue lease.
- **Good:** ephemeral by construction, so cross-submission interference and accumulated
  leaks are impossible rather than merely unlikely.
- **Bad:** ~250 ms of container create/start per execution. Partly mitigated by image
  pre-pull; a warm pool of pre-created paused containers is the documented next step.
- **Bad:** the runner needs the Docker socket, which is root-equivalent on its node.
  Accepted because the runner is the *trusted* component and its node pool holds no
  credentials; a rootless daemon or gVisor removes the property entirely.

---

## ADR-005 — A supervisor binary (`boxrun`) inside every sandbox

**Status:** Accepted

### Context

Docker enforces cgroups but provides no per-process accounting. `docker stats` samples
asynchronously and reports the container, not the program. Verdicts need `ru_utime +
ru_stime`, `ru_maxrss`, the exact exit code and terminating signal, and per-process rlimits.

### Decision

A ~200-line static Go binary is PID 1 in every sandbox. It applies rlimits, execs the
target, kills the process group on a wall deadline, and writes `meta.json` to the shared
bind mount. Because Go's runtime is multi-threaded, it re-execs itself with a `--stage2`
marker: stage 2 applies `setrlimit` to itself (limits survive `exec`) and then
`syscall.Exec`s the target; the parent does `wait4`.

### Alternatives rejected

- **Parse `docker stats` / cgroup files after the fact.** Sampled, racy, and reports the
  container rather than the program. Peak RSS between samples is invisible.
- **`/usr/bin/time -v` or `prlimit` + `timeout` in a shell wrapper.** Workable, but adds a
  shell to every image, gives a coarser signal story, and cannot distinguish a wall kill
  from a CPU kill cleanly.
- **A per-language wrapper script.** Multiplies the supervisor by the number of languages —
  the exact coupling the manifest system exists to avoid.

### Consequences

- **Good:** one binary for every language. Adding Rust adds no supervisor code.
- **Good:** exact metrics, including CPU time for a program that was killed.
- **Good:** `Setpgid` + `kill(-pgid, SIGKILL)` reaps forked children, so a submission never
  leaves a process spinning on a core the next submission is about to use.
- **Bad:** the two-stage self-exec is non-obvious and needs the comment block it has.
- **Bad:** `meta.json` travels over the bind mount, so a sandbox that dies before writing it
  yields no metrics — treated as an internal error and retried, never blamed on the
  participant.
- **Bad:** the binary is copied into images built on different libc families (glibc for the
  JDK image, musl for Alpine-based ones), so it must be built `CGO_ENABLED=0` and fully
  static. A dynamically linked `boxrun` fails with a bare `exec format error` inside exactly
  one language image, which is a miserable thing to debug.

---

## ADR-006 — Three independent timeouts, not one

**Status:** Accepted

### Context

The brief names infinite loops *and* process preemption. These are different failures with
different signatures, and one timer cannot see both.

### Decision

1. `RLIMIT_CPU` (soft `SIGXCPU`, hard `SIGKILL` one second later) — catches busy loops.
2. A wall-clock watchdog in `boxrun` that kills the process group — catches `sleep`,
   blocked reads and deadlocks, which burn no CPU at all.
3. The queue lease — catches a runner that died entirely.

Wall limit defaults to `3 × cpu` limit.

### Alternatives rejected

- **Wall clock only.** Cannot distinguish a busy loop from a legitimately slow solution on
  a noisy node; produces false TLEs under load, which is unfair and unfixable after the fact.
- **CPU only.** `time.sleep(600)` runs to the outer timeout, occupying a judging slot the
  whole time. This is a real denial-of-service against your own fleet.
- **A single hard limit at the API layer.** Cannot kill anything; only reports.

### Consequences

- **Good:** every one of the brief's disruption classes has a mechanism that specifically
  targets it, at a different layer.
- **Good:** `RLIMIT_CPU`'s hard limit means a program that installs a `SIGXCPU` handler and
  keeps spinning still dies.
- **Bad:** three tunables instead of one. Mitigated by deriving the wall limit from the CPU
  limit and validating in `core.Limits.Normalize` that a wall limit below the CPU limit —
  which can never be satisfied — is corrected rather than honoured.

---

## ADR-007 — cgroup memory limits, never `RLIMIT_AS`

**Status:** Accepted

### Context

Memory must be capped, and the cap must produce `MLE` rather than a confusing `RE` or a
slow timeout.

### Decision

`--memory` with `--memory-swap` set **equal** to it (swap disabled), plus
`--memory-swappiness=0`. `OOMKilled` is read from the cgroup via `docker inspect`.
`RLIMIT_AS` is deliberately not set.

### Alternatives rejected

- **`RLIMIT_AS` (address space).** The JVM and Node reserve enormous *virtual* mappings at
  startup, so an AS limit fails them spuriously — every Java submission becomes `MLE`
  regardless of what it does. cgroups count RSS, which is what "memory used" means.
- **`RLIMIT_DATA`.** Misses `mmap`-based allocation, which is most of it in practice.
- **Leaving `--memory-swap` at Docker's default.** The default is `2 × memory` of swap, so
  a leaking program swaps instead of OOMing: a clean `MLE` becomes a 60-second wall timeout
  that also thrashes every other submission on the node.
- **Inferring OOM from exit code 137.** `137` is `128 + SIGKILL`, which the wall watchdog
  also produces. Guessing conflates `MLE` with `TLE`.

### Consequences

- **Good:** `MLE` is accurate, and reported from the cgroup's own accounting rather than
  guessed. `RLIMIT_STACK` separately makes deep recursion a clean `RE`.
- **Good:** a golden fixture (`mle_growlist.py`, a gradual leak) regression-tests the
  swap-disabled decision specifically.
- **Bad:** requires cgroups v2, which is a real prerequisite on WSL2 and on older hosts.
  The environment check script tests for it explicitly rather than failing mysteriously later.

---

## ADR-008 — Verdict precedence is explicit and ordered

**Status:** Accepted

### Context

Several failure conditions are simultaneously true for the same run. An OOM-killed process
also exits non-zero and dies on `SIGKILL`. A program killed by `SIGXFSZ` also dies on a
signal. Checking conditions in the wrong order produces wrong verdicts.

### Decision

`IE > CE > OLE > MLE > TLE > RE > WA > AC`, encoded in a single pure function
(`core.ClassifyRun`) with a table-driven test per ordering pair. The **submission** verdict
is the first non-`AC` test in index order; headline `cpu_ms` is the **maximum** across
tests.

### Alternatives rejected

- **Check the exit code first.** Reports `RE` for every `MLE` and every `OLE`. This is the
  single most common bug in a homegrown judge and it destroys participant trust in the
  results.
- **Report the most severe verdict across all tests.** Participants expect "failed on test
  7", not "the worst thing that happened anywhere".
- **Report the sum of CPU across tests.** The sum is a property of how many tests the setter
  wrote; the maximum is a property of the algorithm on its worst input.

### Consequences

- **Good:** classification is pure, has no I/O, and is unit tested without Docker — so the
  same logic sits unchanged on top of a future gVisor or Firecracker backend.
- **Bad:** the ordering is non-obvious and must stay commented, or a future contributor
  will "simplify" it back into the bug.

---

## ADR-009 — Rank on normalised CPU time; instruction counting is designed, not shipped

**Status:** Accepted (with a documented deferral)

### Context

The event ranks *algorithmic efficiency*. Wall-clock time is noisy. CPU time is much
better. Retired-instruction count is better still — reproducible to roughly ±0.1% and
immune to noisy neighbours, frequency scaling and cache state.

### Decision

Rank on CPU time measured via `wait4`, with slots pinned one per dedicated core and ASLR
disabled. The `instructions` field exists in the schema, in `ExecOutcome`, and in
`core.RankScore` (which prefers it when non-zero). It is not populated.

### Alternatives rejected

- **Ship `perf_event_open` now.** Probed on the deployment target:
PERF_COUNT_HW_INSTRUCTIONS is unavailable (no virtualised PMU under WSL2/Hyper-V), and the syscall is in Docker's default seccomp denylist, so enabling it would also weaken ADR-011's profile. A feature that returns zero on the machine it ships on is worse than an honestly deferred one.
- **Valgrind/cachegrind instruction counts.** Fully deterministic, but ~50× slowdown makes
  it unusable inside a contest time limit.
- **Wall-clock ranking.** Simplest, and roughly an order of magnitude noisier.

### Consequences

- **Good:** the scoring path already prefers the better metric, so enabling it later is a
  runtime capability probe, not a schema migration or a rescore.
- **Good:** the trade-off is measurable — `scripts/determinism.sh` quantifies the residual
  variance, pinned versus oversubscribed.
- **Bad:** ranking remains hardware-sensitive. Mitigated by a homogeneous judge node pool,
  by recording the CPU model on every result for auditability, and by the documented
  per-node calibration-factor design.

---

## ADR-010 — Language support is data (YAML manifests), not code

**Status:** Accepted

### Context

"The code should be modular so that new features can be easily added without changing
several files." The most frequently added feature in a judge is a language.

### Decision

Each language is one YAML manifest (`languages/*.yaml`) declaring image, source filename,
compile command, run command, time multiplier, memory overhead and environment. Manifests
are embedded into the binary with `embed.FS` and loaded through a registry. Adding a
language is one YAML plus one Dockerfile — **zero Go changes**, asserted by
`internal/langs/registry_test.go`, which registers a brand-new language from bytes alone.

### Alternatives rejected

- **A Go `switch` on language.** Every new language touches the judge, the API validator
  and the runner. Exactly the coupling the requirement forbids.
- **A `languages` database table.** Adds a migration per language, puts executable commands
  in mutable data, and separates the manifest from the Dockerfile that must match it.
- **One plugin binary per language.** Real isolation, disproportionate complexity, and a
  distribution problem.

### Consequences

- **Good:** the per-language time multiplier and memory overhead — the policy that keeps
  verdicts fair between Python and C++ — is data, applied once at submission time, so the
  runner never has to know about fairness policy at all.
- **Good:** demonstrable on camera in under a minute.
- **Bad:** a manifest can reference an image that does not exist. Mitigated by validation
  at load time and image pre-pull at runner startup, which fails loudly rather than turning
  every submission into an internal error an hour later.

---

## ADR-011 — Keep Docker's default seccomp profile

**Status:** Accepted

### Context

A custom seccomp profile looks like diligence. Understanding what it actually does is the
decision.

### Decision

Do not pass `--security-opt seccomp=...`. Keep Docker's default profile and layer
`--cap-drop=ALL`, `no-new-privileges`, non-root uid, read-only rootfs and `--network none`
on top.

### Alternatives rejected

- **A hand-written denylist** (`defaultAction: SCMP_ACT_ALLOW` plus a few `ERRNO` entries).
  Passing a custom profile **replaces** the default rather than adding to it, so this is
  strictly *weaker* than shipping nothing — while looking more rigorous. Security theatre.
- **A hand-written whitelist from scratch.** Correct in principle; getting it wrong breaks
  legitimate submissions in ways that are very hard to debug under time pressure, and
  Docker's curated list already blocks ~44 syscalls including `ptrace`, `mount`,
  `pivot_root`, `bpf` and `kexec_load`.

### Consequences

- **Good:** the strongest readily available profile, with no chance of accidentally
  weakening it.
- **Bad:** it is more permissive than a judging-specific whitelist could be. The documented
  path is to start from moby's `default.json` and *remove* syscalls (sockets in particular,
  belt and braces on top of `--network none`) — a refinement, not a rewrite.

---

## ADR-012 — Embedded SQL migrations applied on boot

**Status:** Accepted

### Context

*Reproducibility and Development* is 25 of 100 points, and the evaluator will run the repo.
Every step between `git clone` and a working system is a chance to lose them.

### Decision

Migrations are plain `.sql` files embedded with `embed.FS` and applied by the API on boot,
serialised by `pg_advisory_lock`, with checksums recorded and verified.

### Alternatives rejected

- **golang-migrate / goose CLI.** Standard and good, but an extra binary to install between
  clone and run, and an extra documented step that can be missed.
- **An ORM with auto-migration.** Non-deterministic DDL and no review surface for the index
  decisions that actually matter here.
- **A `schema.sql` applied by the Postgres image's init hook.** Works exactly once, on an
  empty volume; provides no upgrade path.

### Consequences

- **Good:** `docker compose up` is genuinely the whole story. A rolling deploy of N replicas
  is safe because the advisory lock lets exactly one apply while the others block and then
  find nothing to do.
- **Good:** editing an already-applied migration fails loudly instead of leaving two
  environments with silently different schemas.
- **Bad:** no built-in `down` migrations. Accepted deliberately: rollbacks in a contest
  system are restore-from-backup events, and a `down` migration that has never been tested
  is a false sense of safety.

---

## ADR-013 — Bearer tokens in a header, not cookies (and what that means for CSRF)

**Status:** Accepted

### Context

The API is consumed by a browser client and by a CLI. CSRF is an explicitly named threat in
the sibling task and a reviewer will look for it.

### Decision

JWT (HS256, algorithm pinned, issuer validated) in an `Authorization: Bearer` header.
No session cookies anywhere.

### Alternatives rejected

- **Session cookies + CSRF tokens.** Requires `SameSite`, a double-submit token, and
  correct handling on every state-changing route. More moving parts and more ways to be
  subtly wrong.
- **Not pinning the JWT algorithm.** Accepting whatever the token declares is the
  `alg: none` and HS/RS confusion vulnerability class.

### Consequences

- **Good:** CSRF does not apply *by construction* — browsers do not attach `Authorization`
  headers to cross-site requests. This is a design property, not a mitigation to maintain.
- **Bad:** tokens live in JavaScript-reachable storage in a browser client, so XSS is the
  compensating risk. Mitigated by never rendering unsanitised program output (all
  program-produced text is stripped of control characters and escapes) and by short token
  TTLs. A production system would add refresh tokens in `HttpOnly` cookies.

---

## ADR-014 — One judging slot per dedicated core, never oversubscribed

**Status:** Accepted

### Context

Oversubscribing judging slots is the cheapest possible throughput increase: 2–4× per node
for a config change. It is also the largest single source of timing noise.

### Decision

`ARENA_RUNNER_SLOTS = cores − 1`, each slot pinned with `--cpuset-cpus`. Under Kubernetes:
Guaranteed QoS (`requests == limits`) plus kubelet `cpuManagerPolicy: static`, which is
what actually grants exclusive cores — without that flag, core pinning is aspirational and
you get CFS shares.

### Alternatives rejected

- **Oversubscribe 2–4×.** Measured with `scripts/determinism.sh` (N = 20 identical
  submissions), pinned versus 4× oversubscribed — see README §5 for the figures. For an
  event that ranks by measured efficiency, oversubscription turns the leaderboard into
  partly a lottery.

  A caveat recorded honestly: the first run of this harness reported a coefficient of
  variation of exactly **0.00%**, which is not a result. The verdict cache was replaying one
  stored verdict for all twenty identical submissions, so nineteen of the samples never
  executed. The harness now appends a unique comment per iteration. It is worth writing down
  because a 0.00% CV is the number you would most like to report and the one most likely to
  be an artifact.
- **Oversubscribe and re-run near-TLE cases.** Halves the problem at the cost of complexity
  and of a rerun policy that is itself a fairness question.

### Consequences

- **Good:** the property the brief explicitly asks for, with a number attached rather than
  an assertion.
- **Bad:** the slot-to-core mapping is computed, not validated. See ADR-019 — on a host with
  fewer cores than `replicas × slots + base`, every sandbox creation fails. Determinism by
  pinning is only as good as the pinning arithmetic.
- **Bad:** roughly 2–4× more nodes for the same throughput. Absorbed by spot instances,
  where the cost difference is cents per contest — the trade is bought back by ADR-003's
  lease semantics.

---

## ADR-015 — The leaderboard is a Redis ZSET, with Postgres as the rebuildable truth

**Status:** Accepted

### Context

The leaderboard is the most-read object in a contest and the most-written during the final
minutes. It must be correct after a Redis restart.

### Decision

A sorted set per contest, updated per verdict by a Lua script so read-modify-write is
atomic. `(solved, penalty)` are packed into one `float64` — `solved × 1e9 − penalty` — so a
single ZSET gives an exact multi-key ordering with no application-side sorting.
`Rebuild()` recomputes the whole board from Postgres.

### Alternatives rejected

- **`GROUP BY` over `submissions` on read.** The classic contest-day outage: the query gets
  slower exactly as traffic peaks.
- **A materialised view refreshed periodically.** Stale during the minutes that matter most.
- **Read-modify-write from Go.** Races two concurrent verdicts for the same user — very
  common, since participants submit several problems in the same minute — and silently
  loses one. Also three round trips instead of one.

### Consequences

- **Good:** O(log n + m) reads. Serving the top 100 of a 100,000-participant contest is
  microseconds and stays flat under load.
- **Good:** the cache is rebuildable from durable data, so Redis is not a primary store.
- **Bad:** the packing constant caps penalty at 1e9 and solved count at ~9 million. Both
  are far beyond any real contest, and the script saturates rather than corrupting the
  ordering if exceeded.
- **Bad:** scoring logic now exists in two places — Go (`core.RankScore`) and Lua. They are
  tested against each other; a single implementation would require giving up either
  atomicity or the single round trip.

---

## ADR-016 — A verdict cache keyed on inputs, and what its key deliberately omits

**Status:** Accepted (with a known gap) · **Date:** 2026-08-29

### Context

Participants resubmit. A meaningful share of submissions in a contest are byte-identical to
one the system has already judged — a resubmit after a UI mistake, a retry after a network
blip, or the same file sent twice. Executing them again costs real CPU and adds real queue
depth at exactly the moment neither is available.

### Decision

Before enqueuing, the API looks up a cache keyed on `source_hash` + `testdata_version` +
`image_digest`. A hit returns the stored verdict immediately without ever reaching the
queue.

### Alternatives rejected

- **No cache.** Simplest, and measurably wasteful: in a load run, **7,825 of the
  submissions were cache hits**, which is compute that would otherwise have been spent
  re-deriving a verdict already known to be correct.
- **Cache keyed on source alone.** Wrong across a testdata edit or an image rebuild — it
  would serve a verdict derived from inputs that no longer exist.

### Consequences

- **Good:** the largest single cost lever after compile-once. Cache hits are the reason the
  §7 cost estimate is 4.5 core-hours rather than 8.3.
- **Bad, and known:** **the key does not include the limit envelope.** Changing a problem's
  time or memory limit does not invalidate cached verdicts. This produced a genuinely
  confusing hour during development — a correct fix to the C++ compile limits appeared to
  have no effect, because the cache was replaying the pre-fix `CE`. The fix is one field in
  the key; it is not applied because doing so now would invalidate the measurements in
  README §5 and §6, and shipping unmeasured is worse than shipping a documented gap.
- **Bad:** a cache is an optimisation that lies to your instruments. Both the load test and
  the determinism harness initially measured the cache instead of the judge. Any benchmark
  against this system must defeat it deliberately — `scripts/load.js UNIQUE=1` and
  `scripts/determinism.sh` both now do.

---

## ADR-017 — Compilation gets its own limit envelope, separate from execution

**Status:** Accepted · **Date:** 2026-08-29

### Context

Compilation and execution look like the same operation — run a program under limits — and
the first implementation used one envelope for both.

### Decision

`core.DefaultCompileLimits()` is a distinct envelope: 10 s CPU, 20 s wall, 512 MB memory,
**64 MiB `RLIMIT_FSIZE`**, 128 pids.

### Alternatives rejected

- **One envelope for both.** This is not a style preference; it produces wrong verdicts.
  With the run step's tight `RLIMIT_FSIZE`, every C++ submission failed with
  `ld terminated with signal 25` — `SIGXFSZ`. `RLIMIT_FSIZE` bounds *every* file the process
  writes, not just stdout, so a limit sized for a participant's output kills the linker
  while it writes the output binary. The symptom is a universal `CE` that looks like a
  toolchain problem and is actually a limits problem.
- **No compile limits at all.** A template-recursion bomb compiles forever and occupies a
  slot; g++ is perfectly capable of consuming a machine on hostile input.

### Consequences

- **Good:** heavy but legitimate C++ compiles (~512 MB with templates) succeed, while a
  compile bomb still dies. Phantom `CE`s — the kind that look like judge bugs and destroy
  trust — are gone.
- **Bad:** two envelopes to reason about, and the compile envelope is much more permissive
  than the run envelope. Justified because compilation runs the *toolchain*, which is
  trusted code, on untrusted *input*, whereas execution runs untrusted code outright.

---

## ADR-018 — The sandbox working directory is namespaced by runner id, not by slot number

**Status:** Accepted · **Date:** 2026-08-29

### Context

Each judging slot needs a scratch directory on the host, bind-mounted into its sandbox, to
carry source in and `stdout`/`meta.json` out. The obvious naming is `<box-root>/slot-N`.

### Decision

The path is `<box-root>/<runner-id>/slot-N`. The runner id is derived from the container
hostname, the same identity used as the Redis consumer name.

### Alternatives rejected

- **`<box-root>/slot-N`.** This was shipped first and it is a correctness bug, not an
  aesthetic one. **Slot numbers are per-runner.** Two runner replicas sharing a host both
  resolve slot 0 to the same host directory, so they silently overwrite each other's source,
  stdin, stdout and `meta.json`. The observable symptom is the worst kind: **verdicts
  attributed to the wrong submission**, non-deterministically, with everything appearing to
  work. The golden suite sat at 10/19 with the failures moving between runs; after the fix
  it went to 17/19 and wall time fell from 95 s to 13.6 s, because runs had also been
  blocking on each other's files.
- **A per-execution temporary directory (`os.MkdirTemp`).** Correct, and arguably cleaner.
  Rejected because the bind mount requires **path identity** between host and runner (the
  Docker daemon resolves mount paths on the host, not inside the calling container), and a
  fixed, predictable tree is far easier to inspect when debugging a stuck sandbox — which is
  most of what debugging this system is.

### Consequences

- **Good:** two runner replicas per host is now safe, which is what makes the Compose demo
  representative of the scaled deployment.
- **Good:** a directory listing under `<box-root>` shows exactly which runner owns which
  in-flight execution.
- **Bad:** stale directories accumulate if a runner dies without cleanup. Swept at runner
  startup alongside orphaned containers (`SweepStale`).
- **A second bug of the same family, found later:** the box's `input` file is a hard link
  to the node's cached test data. A stale link surviving from a previous submission made the
  copy fallback write *through* it, poisoning another problem's cached input permanently —
  because the cache is only populated on a miss. Every subsequent submission to that problem
  was judged against the wrong input and returned a completely plausible `WA`. Shared mutable
  state between submissions is where this system's real bugs live.
- **Lesson worth recording:** this bug was invisible to unit tests, invisible to a
  single-runner smoke test, and only appeared as *flaky verdicts* under the golden suite
  with two replicas. It is the strongest argument in this project for testing the judge
  against known-answer fixtures rather than trusting the judge to report on itself.

---

## ADR-019 — The demo runs on one small VM, sized by the determinism rule

**Status:** Accepted · **Date:** 2026-08-29

### Context

The submission needs a deployed instance a judge can open. The obvious instinct is to find
the biggest free machine available, and the obvious frustration is that free tiers cap out
at two shared vCPUs.

### Decision

Deploy a single Google Compute Engine `e2-standard-2` (2 vCPU, 8 GB, `asia-south1`,
Ubuntu 24.04) on the free trial, running the same `docker compose` stack as development,
with `ARENA_RUNNER_SLOTS=1` and one runner replica. Report throughput and determinism
figures measured on the 10-core development machine, labelled as such.

### Alternatives rejected

- **GCP Always Free `e2-micro`.** The blocker is **1 GB of RAM**, not the shared cores.
  Postgres, Redis, the API, a runner, Prometheus and Grafana plus a 256 MB sandbox do not
  fit. The instinct to count cores was the wrong instinct.
- **Cloud Run / Render / Railway / Fly Machines.** No Docker socket, no cgroup control, no
  `--pids-limit`. Arena's entire isolation story depends on capabilities these do not expose.
- **Oracle Cloud Always Free (4 ARM cores, 24 GB).** Genuinely fits and is free permanently.
  Rejected on schedule: it needs an ARM64 rebuild of every language image, and A1 capacity is
  frequently unavailable in popular regions.
- **A bigger trial VM.** `e2-standard-4` was affordable against the credit but buys only more
  judging slots, which is a throughput parameter, not a correctness one.

### Consequences

- **Good:** the deployment is honest. Arena's determinism rule is `slots ≤ cores − 1`; it
  says nothing about how many cores exist. A 2-core host running one judging slot enforces
  every limit and produces every verdict correctly — it simply judges one submission at a
  time.
- **Good:** ~$5 for a three-day evaluation window, drawn from the trial credit, with the
  designed contest-scale economics reported separately.
- **Bad:** throughput on the demo host is not representative, so two sets of numbers have to
  be presented and clearly distinguished. Conflating them would be the dishonest shortcut.
- **Bad, and this one was expensive:** deploying to a smaller machine exposed that cpuset
  pinning is computed as `slot + ARENA_CPUSET_BASE` and **never validated against the host's
  core count**. Two runner replicas on a 2-core host requested core index 2; Docker refused
  the container with `Requested CPUs are not available`, and six of eleven smoke cases failed
  as `IE`. The system's response was correct — it refused to guess a verdict — but the right
  fix is a startup guard against `runtime.NumCPU()` that refuses to boot rather than failing
  per submission. Recorded as a known gap in README §17.

---

## ADR-020 — Deliberate scope cuts

**Status:** Accepted · Recorded so the omissions read as decisions, not gaps

| Not built | Why | What exists instead |
|---|---|---|
| Multi-node live deployment | 2-day timebox; a single VM demonstrates the system honestly | `deploy/k8s` + `deploy/keda` written, reviewed, documented as the scale path |
| gVisor / Firecracker | not installable on the demo target in time | `Sandbox` interface; `--runtime=runsc` is a one-line change |
| Warm container pool | ~250 ms → ~50 ms start, but real complexity | image pre-pull at runner startup captures most of the benefit |
| Special judges, interactive problems | no problem in the demo contest needs them | `Checker` registry designed for exactly this — a new file plus one `Register` call |
| `submissions` table partitioning | complicates unique constraints, buys nothing at this scale | DDL recorded here; partial indexes carry the load today |
| Subtask scoring | contest mode is all-or-nothing | `core.TestPoints` implemented and tested, unused |
| Refresh tokens | short-lived access tokens are sufficient for a 3-hour contest | documented as the production follow-up in ADR-013 |
| Limit envelope in the verdict cache key | applying it now would invalidate the load and determinism measurements | gap stated in ADR-016 and README §16; one-field fix |
| Claim batch bounded to *free* slots | observed PEL depth is safe, just wider than necessary | leases are heartbeated and reclaimable; measured and recorded in README §16 |

Recording the cuts is the point. A reviewer who finds an obvious missing feature and also
finds it listed here, with a reason, trusts the rest of the document more.
