# zfstui — agent notes

TUI/CLI for managing ZFS dataset replication across ssh-reachable hosts.
READ DESIGN.md first: it has the architecture, the decisions log, and the
roadmap (M1 discovery + M3 read-only TUI are done; next is M2 sync planner,
then M4 jobs/actions). README.md has the original wishes + status.

## Commands

```
go build -o zfstui . && go vet ./... && go test ./...   # all must stay green
gofmt -l .                                              # must print nothing
./zfstui refresh root@ws ivan@cml                       # real ssh discovery (read-only on hosts)
./zfstui status [-a] [host...]                          # matrix from cache
./zfstui [host-spec...]                                 # TUI
```

Host model (2026-06-11, "never again" bug class): the **hostname is the sole
identity** — `root@ws` and `ws` are the same host. Specs are positional args
(`host`, `user@host[:pool]`, `ssh://user;send_user=x;recv_user=y@host[:port]
[/pool]` — chrome-style `;opt=value` userinfo options). Parsed/normalized
access fields are remembered in the sqlite hosts table and replaced wholesale
by new specs; no specs → use remembered hosts; specs → limit to them.
Conflicting specs for one host in one invocation → error. Default user is
root; **non-root user implies sudo** (no sudo option exists). There is **no
config file** (TOML inventory was dropped 2026-06-11). The pool path part is
parsed/stored/displayed, not yet enforced.

Dev-mode default (deliberate, revert to XDG before release): sqlite cache
`./zfstui.db`, cwd-relative so the user can run and the agent can inspect the
same file. It is disposable (cache + remembered hosts); a schemaVersion bump
in store.go wipes and recreates it — no migrations, ever. The user's real
fleet db may be present in cwd; `./zfstui status` works offline from it.

## Hard rules (data safety — the project's #1 constraint)

Only ever emit: `zfs snapshot`, `zfs send | zfs recv -s -u` (NEVER `-F`),
`zfs destroy <ds>@<snap>` (prune only), `zfs receive -A` (explicit abandon).
Never: dataset/clone destroy, rollback, rename, `destroy -R/-f`, zpool or
property mutations. ALL mutating command lines are built in mutate.go and
nowhere else (charset validation + shell quoting); mutate_test.go/jobs_test.go
assert constructor output and every mutating job-script line match the
allowlist regex. Extend the regex + constructors together. The global
`-dry-run` flag must keep disabling every mutation path.

## Code map (flat package main, one dir — user explicitly wants no subdirs)

- `model.go` — types: Host (identity=Name + access spec; EffUser/NeedSudo/
  Dest/URL helpers), Pool, Dataset, Snapshot, HostState (per-host raw
  inventory), Replica/Group (derived). No logic beyond tiny helpers.
- `hostspec.go` — parseHostSpec/parseHostSpecs (ssh/scp URL forms, conflict
  rejection, root-user normalization to "") and resolveHosts (specs → sqlite
  memory; specs limit + replace stored access).
- `zfsparse.go` — parsers for `-H -p` tab-separated output ("-" = absent) +
  `DiscoverCommand` (one ssh round-trip: three sections with `===ZFSTUI:*===`
  markers and per-section exit codes) + `ParseDiscovery` splitter.
- `ssh.go` — `runner` interface (everything testable offline via fakes);
  real impl shells out to system `ssh` (BatchMode, ConnectTimeout,
  ControlMaster sockets in ~/.local/state/zfstui/cm). Never use an ssh lib.
- `store.go` — sqlite cache (modernc.org/sqlite). `db.SetMaxOpenConns(1)` is
  load-bearing: parallel refresh writers otherwise hit SQLITE_BUSY, and
  `:memory:` would mean one DB per pool connection.
- `derive.go` — THE core; pure functions of inventory:
  - groups = union-find over shared **snapshot** GUIDs (+ clone `origin`
    links). Dataset GUIDs differ per replica by design — never use them for
    identity; snapshot GUIDs survive send/recv and are the cross-host truth.
  - display name = longest common path suffix (pool stripped) of members,
    clones excluded; leading components whose ancestor dataset is
    snapshot-less ("bak"/"srv" plumbing) are trimmed (`trimBoring`); whatever
    isn't in the group name shows as the per-cell prefix.
  - states via pairwise compare against newest shared snapshot: ahead /
    behind / diverged (both sides ahead) / in-sync; `written>0` counts as
    ahead; clones and resume tokens override display state. No stored
    "master", no pins — source is always derived (DESIGN "Source detection").
  - `Group.Hidden` = no member has any snapshot (plumbing datasets).
- `mutate.go` — THE ONLY place mutation command lines are built (see Hard
  rules). Currently: buildSnapshotCmd.
- `jobs.go` — remote job protocol: /tmp/zfstui-job-<id>/ dirs (job.sh,
  meta.json written by job.sh itself, status, log), one-round-trip launch
  with job.sh on ssh stdin + setsid, waitJob status polling. snapshotJob is
  the first job kind.
- `ops.go` — `Refresh` (errgroup fan-out, limit 8; unreachable hosts are
  recorded, cache kept) and `loadData` (cache → Data{States, Groups, Hosts,
  SnapsByReplica}).
- `statusview.go` — plain-text matrix for `status`. Cell markers: `+` written
  `!` diverged `~` resumable `%` clone; ahead deliberately has NO marker
  (user prefers @names to speak); newest @name in a row = the source.
- `tui.go` — bubbletea+lipgloss. Cursor coordinate system: `curRow == -2`
  host header row (←/→ jump host-to-host, span highlighted), `-1` pool header
  row, `0..` group rows; `curCol == 0` name column (group details), `1..N`
  data columns into sorted `cols` ("host:pool"). Two-line header: hosts
  (underlined, spanning) over pools (bold). `focus` 0=Datasets 1=Details
  (Tab toggles; Details scrolls via `detOff`). `h` toggles hidden groups —
  NOT vim-left (user decision). Refresh runs as tea.Cmd, results via
  `refreshDoneMsg`. `s` opens the snapshot dialog: a centered lipgloss-
  bordered modal form composited OVER the table (overlayCenter, ANSI-aware
  splicing) with bubbles textinput + checkbox + buttons (Tab/↑↓ cycle focus,
  Space toggles/presses, Enter = default action). The form previews the
  exact command live and Create acts immediately — no separate confirm
  screen (user decision); in -dry-run the button reads "Create (dry run)"
  and opens a scrollable centered `textModal` with command/job dir/job.sh
  instead of executing. `m.modal` (textModal) is the generic scrollable
  text modal (also used for job-failure logs). Reuse button()/checkbox()/
  styleDialog*/overlayCenter/textModal for future dialogs (sync, prune).
  Dialog state machine is unit-tested without a pty (tui_test.go drives
  Update with tea.KeyMsg directly; throwaway render tests for eyeballing
  layout are in git history).
- `main.go` — flag parsing (global flags BEFORE command), command dispatch;
  first positional arg is the command iff it matches one, the rest are host
  specs. `status` works from cache alone; the TUI resolves hosts from sqlite
  so refresh always works (no "refresh disabled" states).

## Conventions & user preferences (learned, don't regress)

- No naming-convention config or hardcoded `srv/`/`bak/` anywhere; everything
  display-related is derived from data.
- Keep deps minimal; parse zfs output by hand; stdlib `flag`, no cobra.
- Minimal cell markers; no master indicator; usernames not shown in table.
- Tests: table-driven pure-function tests (see derive_test.go `fx` builder,
  hostspec_test.go for spec parsing/resolution), fixture text for parsers,
  fakeRunner for end-to-end (main_test.go). New derive/planner logic must
  come with scenario tests.
- TUI can't be keyboard-tested here: pty via `script` renders one frame but
  key delivery is unreliable — verify render with
  `timeout 6 script -qec "stty rows 32 cols 120; ./zfstui" /dev/null >/tmp/t.txt`
  + strip ANSI, and ask the user to drive interactively for behavior.
- The user commits; agent cannot (`.git` is read-only here). Keep README.md
  and DESIGN.md updated as you work — explicit user requirement.

## Known warts / next steps

- Hosts table keeps entries for hosts you stop using (and `status <typo>`
  remembers the typo) until the db is wiped — a `forget` command may come.
- Real-data oddity to remember: `packages` appears as two unrelated groups
  (no shared snapshot GUIDs) — correct but user may ask about re-linking
  (needs full resend; relevant for M2 messaging).
- M2 next: sync planner + compatibility gate (DESIGN "Sync planning") +
  `sync --dry-run`; pure functions + tests first, then `doctor`.
- M4 partly done: job mechanics + TUI snapshot dialog + -dry-run. Still
  missing: batch `snap` command, refresh-time job discovery (glob
  /tmp/zfstui-job-* into a jobs table/pane), sync execution, prune.
