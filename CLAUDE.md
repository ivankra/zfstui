# zfstui — agent notes

TUI/CLI for managing ZFS dataset replication across ssh-reachable hosts.
READ DESIGN.md first: it has the architecture, the decisions log, and the
roadmap (M1 discovery + M3 read-only TUI are done; next is M2 sync planner,
then M4 jobs/actions). README.md has the original wishes + status.

## Commands

```
go build -o zfstui . && go vet ./... && go test ./...   # all must stay green
gofmt -l .                                              # must print nothing
./zfstui -hosts root@ws,root@cml refresh                # real ssh discovery (read-only on hosts)
./zfstui status [-a]                                    # matrix from cache
./zfstui                                                # TUI
```

Dev-mode defaults (deliberate, revert to XDG before release): config
`./zfstui.toml`, sqlite cache `./zfstui.db` — both cwd-relative so the user
can run and the agent can inspect the same files. `zfstui.db` is a pure,
fully-rebuildable cache; `rm` it freely (it keeps rows for hosts no longer in
the inventory — that's a known wart, see "forget" idea in chat history).
The user's real fleet db may be present in cwd — `./zfstui status` works
offline from it; never needs ssh.

## Hard rules (data safety — the project's #1 constraint)

Only ever emit: `zfs snapshot`, `zfs send | zfs recv -s -u` (NEVER `-F`),
`zfs destroy <ds>@<snap>` (prune only), `zfs receive -A` (explicit abandon).
Never: dataset/clone destroy, rollback, rename, `destroy -R/-f`, zpool or
property mutations. When M2/M4 add command generation, build all mutating
command lines in one place and golden-test them (DESIGN "Data safety").
Everything implemented so far is read-only (`zpool list`/`zfs list`).

## Code map (flat package main, one dir — user explicitly wants no subdirs)

- `model.go` — types: Host, Pool, Dataset, Snapshot, HostState (per-host raw
  inventory), Replica/Group (derived). No logic beyond tiny helpers.
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
  `refreshDoneMsg`.
- `main.go` — flag parsing (global flags BEFORE command), command dispatch.
  `status` must never require config/inventory; only ssh-ing commands load it.
  Inline `-hosts user@host` → User=user, Name=host (name is identity/column
  label; user is access-only and never displayed).

## Conventions & user preferences (learned, don't regress)

- No naming-convention config or hardcoded `srv/`/`bak/` anywhere; everything
  display-related is derived from data.
- Keep deps minimal; parse zfs output by hand; stdlib `flag`, no cobra.
- Minimal cell markers; no master indicator; usernames not shown in table.
- Tests: table-driven pure-function tests (see derive_test.go `fx` builder),
  fixture text for parsers, fakeRunner for end-to-end (main_test.go). New
  derive/planner logic must come with scenario tests.
- TUI can't be keyboard-tested here: pty via `script` renders one frame but
  key delivery is unreliable — verify render with
  `timeout 6 script -qec "stty rows 32 cols 120; ./zfstui" /dev/null >/tmp/t.txt`
  + strip ANSI, and ask the user to drive interactively for behavior.
- The user commits; agent cannot (`.git` is read-only here). Keep README.md
  and DESIGN.md updated as you work — explicit user requirement.

## Known warts / next steps

- Hosts table keeps stale entries (e.g. old `root@ws` names) until db wiped.
- `status` column headers come from cached host names; user must re-refresh
  after the user@host change to see clean names.
- Real-data oddity to remember: `packages` appears as two unrelated groups
  (no shared snapshot GUIDs) — correct but user may ask about re-linking
  (needs full resend; relevant for M2 messaging).
- M2 next: sync planner + compatibility gate (DESIGN "Sync planning") +
  `sync --dry-run`; pure functions + tests first, then `doctor`.
- M4 after: job dirs on source host (DESIGN "Remote job protocol"), snapshot/
  sync dialogs, jobs/log panes.
