# zfstui — maintenance / refactoring proposals

Status: **open items only.** This doc tracks the remaining maintenance/refactor
candidates. Completed items have been removed as they landed — see the DESIGN.md
decisions log (and git history) for what was done. Goal: a simpler, smaller,
more maintainable codebase without regressing the data-safety guarantees or the
field-tested behaviour.

The 2026-06-16 pass cleared all the easy/safe items (helper extractions, the
`colKey` refactor, several correctness nits) and removed the `-dry-run` feature
entirely. What's left below is medium+ effort, needs a product decision, or was
deliberately deferred.

Rough sizes for context (LOC): tui.go 2134, view.go 1392, zfsparse.go 768,
store.go 388, derive.go 375, model.go 283, sync.go 189, jobs.go 176,
main.go 156, mutate.go 151, ops.go 150, details.go 121, statusview.go 102,
ssh.go 85.

---

## Overview & tracker

Status legend: ☐ todo · ◐ in progress · ⏸ deferred · ℹ info-only.

### Refactorings (A)
| ID | ☐ | Item | Value | Effort | Notes |
|----|---|------|-------|--------|-------|
| A3  | ☐ | de-dup selection→view classification (`currentView` vs Enter) | med | med | pairs w/ the A16 helper |
| A7  | ☐ | shared dialog focus/render glue — **stay minimal** | med | med | no form framework |
| A8  | ☐ | split `tui.go` (2134 LOC) into table/dialogs/jobs/format | med (nav) | low | do last, flat pkg |
| A17 | ☐ | extract sync-submission planning out of TUI (pure/testable) | med | med | reusable by batch CLI |
| A18 | ☐ | declarative `discoverySection` table | high | med | enables doctor/cleanup |
| A19 | ☐ | inject Runner-factory + clock for testable launch | med (tests) | med | with A8 |

### Bugs / nice-to-fix (B)
| ID | ☐ | Bug | Severity |
|----|---|-----|----------|
| B1 | ☐ | main-table `CellText` shows no dirty marker; classify lets clone/resumable preempt modified | low / **confirm intent** |
| B3 | ⏸ | `sizeVariant` string-rewrites a command in the safety layer | low / invariant — **deferred** (safety file; needs a non-churny design) |
| B5 | ℹ | no per-attempt deadline inside `waitJob` (callers have timeouts) | informational |
| B7 | ⏸ | `planSync` accepts resume token without verifying target GUID | med (+test) — **deferred** (needs a ZFS resume-token decoder) |

### Security / data-safety (C)
| ID | ☐ | Item | Action |
|----|---|------|--------|
| C3 | ☐ | mutate.go regex/test lockstep | process note |
| C4 | ☐ | `send_user`/`recv_user` parsed/stored/shown but **unused** | **DECIDED: drop them** (schema bump) |
| C5 | ☐ | custom sync dest to unknown host → silent `root@host` | require known host / full spec |
| C6 | ☐ | host-spec `:pool` not enforced | **DECIDED: enforce** — `Host.Pools` set, merge on repeat, error if bare-repeat |

### Suggested order
Mechanical/structural next: **A18** (declarative discovery — enables doctor /
job-dir cleanup) → **A17 + A7** (sync-submit extraction + form glue) → **A8**
(+ A19) the file split, last. **C4** is decided and low-risk (mechanical, ~6
files + schema bump). **B1 / C5** want a product decision first; **C6** is a
real feature. **B3 / B7** are deferred; **B5** is informational.

Each step: `go build -o zfstui . && go vet ./... && go test ./... && gofmt -l .`
must stay green, and rebuild the binary the user runs.

---

## A. Refactorings (simplify / shrink)

### A3. De-duplicate the selection→view mapping (currentView vs the Enter handler)
`details.go:currentView()` and the main-table `Enter` switch (tui.go) BOTH map
(curRow,curCol,selected cell)→object. They diverge on purpose (Enter pushes a
window / focuses the pane; currentView returns the pane view), but the
*classification* (host header / pool header / name col / single vs multi-replica
cell) is duplicated and can drift.

**Proposal:** extract `func (m *Model) selectionKind() (kind, host, pool, *Group, *cell)`
or have the Enter handler call `currentView()` and only decide push-vs-focus
from the returned view's type. At minimum, share the header/col classification.
(The A16 `selectedReplicaFor` helper covers the *dialog* source resolution; A3
is the pane/Enter classification — different call sites.)

**Impact:** removes a divergence trap (we already had to special-case the group
column twice). Medium risk (interaction-heavy) — lean on tui_test.go.

### A7. Pull the form-dialog scaffolding shared by snap & sync dialogs
`snapDialog` and `syncDialog` both implement: focus index + `setFocus`,
Tab/↑↓ cycle, Space-toggles, Enter-submits, `dialogBox`+`button`+`checkbox`
rendering. The two `updateXDialog`/`submitXDialog`/`XDialogView` trios are large
and parallel. (Submit paths simplified once `-dry-run` was removed — A7 is now
just focus + render glue.)

**Proposal:** extract a tiny `formFocus` helper (cycle/clamp + which-widget-is-
focused, ideally mapping key names → next/prev/submit/cancel/toggle intents).
**Keep it deliberately small** — do NOT build a generic form framework or
abstract the field layouts; the snap and sync forms differ enough that a shared
layout would be worse. Share only focus movement and maybe `button`/`checkbox`
rendering.

**Impact:** meaningful LOC reduction in the largest file; the two dialogs stay
readable. Medium effort, medium risk — dialogs are interaction-heavy but
tui_test.go drives them. (Both reviews stress: stay minimal here.)

### A8. Split tui.go (2134 LOC) along seams
tui.go mixes: styles, Model/state, key dispatch, the table renderer
(`tableView`, ~250 LOC), the two dialogs, helpers (`humanShort`, `pad`,
`fillBar`, `fmtUptime`, …), and tick/poll plumbing.

**Proposal (low-risk, no logic change):** move the dialogs to `dialogs.go`,
the table renderer to `table.go`, and the small formatting helpers to
`format.go`. Package stays flat (user requirement: one dir), this is just file
organisation. (CODEX's finer split: `tui_update.go`/`tui_table.go`/
`tui_dialog_snapshot.go`/`tui_dialog_sync.go`/`tui_jobs_live.go`, keeping
`tui.go` for Model+styles+construction.)

**Impact:** navigability; smaller review surface per file — notably separates
the safety-sensitive job-launch paths from visual-layout churn. Zero behaviour
change. Do AFTER A7 so the moved code is already simplified.

### A17. Move sync-submission planning out of the TUI
`submitSyncDialog` (tui.go) validates checked plans, parses custom destinations,
rejects existing datasets, builds jobs, sets UI state, AND starts execution — a
lot of behaviour in one UI method. (It shrank a bit when the dry-run output
branch was removed, but the build/validate logic remains.)

**Proposal:** a pure `sync_submit.go` function turning (checked plans + custom
dest text) → `[]jobSpec` + labels + affected hosts. The TUI method only maps
form state to it, then calls `runSyncJobs`. (A pure plan-printer here is also
the natural home for any future batch-CLI command preview.)

**Impact:** shrinks the largest file, and the extracted function is directly
reusable by the future batch `sync` CLI and unit-testable without driving Bubble
Tea (custom-dest edge cases especially). Medium effort; pairs naturally with A7.

### A18. Make discovery sections declarative
`DiscoverCommand` (zfsparse.go) builds one long shell string inline; the marker
protocol has already evolved once (the `==J==`→base64 rework). The marker set is
also spelled out twice (the section switch + the required-list slice).

**Proposal:** `type discoverySection struct{ marker, command string; sudo, required bool }`
and a `var discoverySections []discoverySection`. `DiscoverCommand` renders from
it; `ParseDiscovery` reuses the same list for its required-section check (this
also kills the duplicated marker list).

**Impact:** adding `doctor` / job-dir cleanup / optional platform probes no
longer means editing a long join; required-vs-best-effort becomes data, not
prose. Medium effort, medium risk (it's the discovery round-trip — lean on
zfsparse_test.go fixtures).

### A19. Inject Runner factory + clock into job execution
`runSnapshotCmd`/`runSyncJobs`/job polling call `newSSH()` and use wall-clock
durations directly. Tests already cover a lot via fakeRunner where it's threaded,
but these launch paths instantiate the real one.

**Proposal:** a small `runtimeDeps{ newRunner func() Runner; tick time.Duration }`
on `Model` with production defaults, overridable in tests.

**Impact:** unit-testable launch-failure / polling-transition / live-submit
behaviour (now that dry-run is gone, a fake runner is the *only* way to test the
submit paths without real ssh — see the reworked dialog-flow tests), and a seam
for the future job-cancel. No user-facing change. Medium effort; do alongside
A8's `tui_jobs_live.go` split.

---

## B. Bugs / correctness nits

### B1. `clone`/`resumable` dirty marker on the main table — confirm intent
`derive.classify` lets `StateClone`/`StateResumable` preempt `StateModified`, so
a clone/resumable replica with uncommitted writes is classified `clone`/
`resumable`, not modified. Separately, `CellText` (the main-table cells) never
renders a dirty marker for *any* state — it's color-only, and color can't encode
clone+dirty. (The detail/replica STATUS column is fine: it appends `+` when
`Written>0`.)

**Decide:** should main-table cells distinguish a dirty clone/resumable? If yes,
adjust `CellText` + `classify` (and add tests). Low priority / likely by design.

### B3. `sizeVariant` does a textual `strings.Replace("zfs send ", …, 1)` — DEFERRED
mutate.go. Robust today (every send starts with `zfs send `/`sudo -n zfs send `
and the first match is the real command) but it's a string-rewrite of a command
line — the exact class of thing the rest of mutate.go avoids by *constructing*.

**Deferred** (this is the #1 safety file). The obvious fixes are unattractive:
adding `sizing bool` makes `buildSendCmd` take FOUR positional bools (sudo,
sizing, raw, intermediate) — error-prone at the 8 mutate_test.go + 3 sync.go call
sites — and anchoring the string surgery to the known prefix is no improvement.
**Proposed clean design when picked up:** give the send builders an internal
`sendHead(sizing) → "zfs send"|"zfs send -nP"` and either (a) return both the
real and sizing command (two-value return, since syncJob always wants both), or
(b) thread a small `sendOpts{sudo, raw, sizing}` struct to kill the positional
bools. Then delete `sizeVariant` and have `syncJob` ask the builder for the
sizing twin. Extend mutate_test.go's `-nP` allowlist regex in lockstep (C3).

### B5. No per-attempt timeout inside `waitJob` — informational
The launch/poll paths DO carry contexts (snapshot 90s, sync 10m, live poll 15s,
tick 5m). The only gap is that a single ssh attempt inside `waitJob` relies on
ssh's own ConnectTimeout, not a per-iteration deadline. **Informational —
revisit only if hangs are observed.**

### B7. `planSync` accepts a resume token without verifying its target — DEFERRED
sync.go — `case d.ResumeToken != "": planResume` marks any non-empty token
resumable without checking that the token's target snapshot GUID is present on
the source (DESIGN says it should). Likely non-destructive (`zfs send -t` fails
on an invalid token), but the UI can offer a doomed resume job.

**Deferred:** the fix needs to decode the token's GUID (a ZFS resume token is a
gzip-compressed nvlist — a non-trivial, fragile parser) and gate on its presence
in the source GUID set; fall through to incompatible/incremental otherwise. Add
a planner test (token decodes to a GUID absent from source ⇒ not `planResume`)
when the decoder lands. Clean test target in sync_test.go.

---

## C. Security / data-safety observations

Overall the data-safety design is strong: all mutations funnel through mutate.go
with charset validation + shell quoting, and `mutate_test.go` / `jobs_test.go`
assert the emitted shapes against an allowlist. Keep that.

### C3. Validation regexes and constructors must extend together — process
mutate.go (datasetRe/snapNameRe/resumeTokenRe) + mutate_test.go allowlist are
coupled by convention. If A3/A7/B3 move any command assembly, the test scanner in
jobs_test.go must still see every shape. **Process note, not a defect.**

### C4. `send_user` / `recv_user` — **DECISION: DROP THEM**
The `ssh://…;send_user=x;recv_user=y@host` options round-trip through
hostspec.go → store → the host detail view, but sync execution never consults
them (verified: no `SendUser`/`RecvUser` reader outside parse/store/display).
Rather than wire up a feature nobody is using, **remove them entirely.**

**Work to do (when scheduled):**
- model.go: drop `Host.SendUser`/`RecvUser` fields; remove from `URL()` and
  `SameAccess()`.
- hostspec.go: remove the `send_user`/`recv_user` cases in the `ssh://` userinfo
  parser; an unknown option becomes a plain error (update the comment block and
  the "known options" error text — there are then no known options, so reject any
  `;opt=`).
- store.go: drop the `send_user`/`recv_user` columns from the `hosts` table, the
  INSERT/UPDATE, and the SELECT/Scan. **Bump `schemaVersion`** (disposable cache
  wipes & recreates — no migration).
- view.go: remove the send_user/recv_user lines from the host ssh chip.
- main.go: drop them from the `ssh://` usage example if shown.
- Update DESIGN.md + CLAUDE.md (the host-model paragraph mentions the `;opt=value`
  userinfo options and `send_user=x;recv_user=y`).
- Tests: hostspec_test.go / store_test.go likely assert these — update.

**Impact:** removes a misleading no-op feature; simpler Host model + schema. Low
risk (schema bump handles the DB). Medium-small effort (touches ~6 files + tests).

### C5. Custom sync destinations to unknown hosts silently default to `root@host`
`submitSyncDialog` accepts free-form `host:pool/dataset` custom destinations, and
`hostByName` falls back to `Host{Name: name}` when the host isn't in the active
list — i.e. default user `root`, no remembered access options. Typing a
not-yet-known host silently targets `root@host`.

**Fix:** require custom-dest hosts to be remembered/active, or accept a full
host-spec syntax in the editbox and persist it explicitly. Surprising-access
risk; data-safety-adjacent. (Wants a small product decision on which.)

### C6. Host-spec pool component — **DECISION: ENFORCE IT**
`host:pool` (and `ssh://…/pool`) currently parses/stores/displays the pool but
doesn't scope anything. **It should LIMIT the host to just that pool**, with
multi-pool support via repetition:

**Required semantics (CLI spec resolution):**
- `zfstui ws:tank` → host `ws` limited to pool `{tank}` (datasets/snapshots/
  pools/vdevs outside `tank` are excluded from columns, groups, status).
- `zfstui ws:tank ws:dpool` → **merge** → `ws` limited to `{tank, dpool}`.
- `zfstui ws` (bare) → `ws` **unrestricted** (all pools).
- **`zfstui ws:tank ws`** (repeat where one occurrence omits the pool) →
  **ERROR** — ambiguous (restrict-to-tank vs no-restriction). Likewise a bare
  host repeated (`ws ws`) is a repeat-without-pool → error.
- Access (user/port) must still agree across occurrences (existing conflict
  rule), independent of pools.

**Model change:** `Host.Pool string` → `Host.Pools []string` (a set; empty =
unrestricted). `parseHostSpec` yields 0-or-1 pool; `parseHostSpecs` merges per
host and enforces the error rule above. `URL()`/`SameAccess()`/store schema
updated (sets serialised, e.g. comma-joined or a join table — **bump
`schemaVersion`**). Add `func (h Host) AllowsPool(pool string) bool`
(empty Pools ⇒ true).

**Enforcement point:** filter the loaded inventory by allowed pools so derive
never sees excluded data. Cleanest seam: in `loadData` (ops.go) after
`AllHostStates()`, prune each `HostState`'s Pools/Datasets/Snapshots/VDevs to
`h.AllowsPool(...)` for that host (Disks are host-level — keep, or drop those
mapping only to excluded pools' vdevs). `loadData` currently takes
`only []string` (names); it'll need the resolved `[]Host` (or a
`map[host][]pool`) to know the restrictions. Refresh still discovers everything
in one round-trip; restriction is a *view* filter, not a discovery filter
(keeps the cache complete and lets the restriction change without a re-refresh).

**Tests:** hostspec_test.go (merge + error cases), a loadData/derive filter test
(restricted host shows only its pools' groups).

**Impact:** real feature; medium effort, touches model/hostspec/store/ops +
tests. Medium risk (changes the active-set semantics) — well-isolated by tests.
Update DESIGN.md "known warts" (remove the "not enforced" note) + CLAUDE.md.
