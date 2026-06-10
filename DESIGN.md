# zfstui — Design

Status: **draft for discussion** (nothing implemented yet). See README.md for the
original wishes; this document is the proposed answer. Open questions at the bottom.

## One-paragraph summary

A single static binary (`zfstui`) running on a *control node* (your laptop). It
ssh-es into a configured list of hosts, scrapes `zpool`/`zfs` machine-readable
output into a local sqlite cache, correlates datasets across hosts **by snapshot
GUID** (not by name), and presents a hosts×datasets matrix in a TUI (plus batch
CLI subcommands). Replication is executed by generating a self-contained bash
job script and launching it detached (`setsid`) on the source host, so transfers
survive the control node disconnecting; job status/log/progress live in files on
that host and are picked up on the next refresh — even by a brand-new control node.

## Key decisions

### Identity: snapshot GUIDs, not names

`zfs send | zfs recv` preserves the `guid` property of snapshots. Therefore:

- Two datasets (on any hosts/pools) belong to the same **replication group**
  iff they share at least one snapshot GUID. Groups = connected components of
  the "shares a snapshot GUID" graph.
- Names like `ssd/srv/git` vs `tank/bak/git` are only used for *display*, and
  no naming convention is configured or hardcoded: the group's display name is
  the **longest common path suffix** of its replicas' names (pool stripped) —
  `git` for the above — and each table cell shows that replica's remaining
  prefix (`srv/`, `bak/`). Conventions like `srv/`/`bak/` thus render exactly
  as in the README illustration without the tool knowing about them. If there
  is no common suffix, the full name is shown per cell.
- Additionally, leading components whose ancestor dataset is **snapshot-less
  plumbing** (a bare `bak`/`srv` container — the same datasets the hidden
  rule classifies) are trimmed from display names even when *all* replicas
  share them (`bak/anki` everywhere → group `anki`, cells `bak/@…`), so
  related rows sort together. Trimming stops at the first ancestor with its
  own snapshots (a real replicated parent like `blobs` of `blobs/01`) and the
  trimmed part still shows in cell prefixes.
- This automatically keeps same-named-but-unrelated datasets (`rpool`, `cache`)
  apart, and tolerates renamed/odd-prefixed replicas.
- Clones are detected via the `origin` property and shown annotated/indented
  under their group, but treated as their own dataset (no special sync logic
  initially — keep it simple).
- Hierarchical datasets (`blobs`, `blobs/01`, …): each child is its own group
  and its own table row; the parent dataset (itself a real dataset on the
  hosts) gets its own row too. Rows sort lexicographically by display name, so
  the parent naturally sits above its children. No collapsing/aggregation in
  v1 — only `-r` flags on snapshot/send where the user asks for them.

### Source detection: derived from data, not declared

There is no stored "master" role, no pins, no name heuristics. Per group, a
replica is **ahead** if it has data the others lack past the newest common
snapshot: snapshots with GUIDs no other replica has, and/or `written > 0`
(local writes since its latest snapshot). Then:

- exactly one replica ahead → it's the source; sync defaults flow from it
- none ahead → group is in sync; no sync needed, "master" is moot
- two or more mutually ahead → **diverged**: flagged red. This does *not*
  disable syncing for the group — the sync dialog (or `--from`/`--to`) can
  still replicate between any pair that is compatible (see the compatibility
  gate in Sync planning); only pairs whose receive would be destructive are
  rejected, with the reason shown. Repairing the diverged pair itself stays
  a manual job.

Wrong-direction replication is structurally impossible this way: the tool only
ever proposes sending from the replica that provably has the newer data.

Caveat worth documenting for users: `written` counts *any* writes, including
atime updates on a mounted replica, which would surface as false divergence.
That's honest (the replica really did change) and the usual setups
(`canmount=off` backups, `atime=off`) avoid it; the diverged-cell detail view
shows the `written` byte count so a 4K-atime-blip is easy to recognize.

### Hidden groups

Groups where **no member has any snapshot at all** are hidden by default —
these are almost always system/plumbing datasets (container datasets like a
bare `bak`/`srv`, pool roots, OS datasets), not replication subjects. `status
-a` and the TUI `h` key toggle showing them. Hidden is purely computed (no
persisted per-dataset flags — keep it stateless); hiding is pure display, and
hidden groups still refresh and still count for safety checks.

### Per-replica freshness (drives table cell rendering)

For each replica in a group, comparing against the group-wide latest snapshot
(by `creation` timestamp, since names aren't guaranteed sortable):

| state     | meaning                                                           | render |
|-----------|-------------------------------------------------------------------|--------|
| in-sync   | has the group's latest snapshot, `written` == 0                   | `bak/@20260601` plain/green |
| ahead     | sole holder of newest data (unique snaps and/or `written` > 0)    | `srv/@20260610` bold (`+` if unsnapshotted writes) |
| behind    | missing the group's latest snapshot, no unique data of its own    | yellow, `@<its-latest>` |
| diverged  | unique data past the common base while another replica is ahead too | red `!` marker |
| resumable | has a `receive_resume_token` (interrupted recv)                   | cyan `…` marker |

### Language: Go (bubbletea + pure-Go sqlite)

Chosen over Rust because the owner is more at home in Go — long-term
hackability of the tool by its user beats marginal library preferences.

- TUI: `bubbletea` + `lipgloss` (+ `bubbles` for table/inputs where they fit).
- sqlite: `modernc.org/sqlite` (pure Go — keeps cross-compilation and the
  single-static-binary story trivial; no cgo).
- Config: `BurntSushi/toml`. CLI: stdlib `flag` with manual subcommand
  dispatch — no cobra; the command surface is small.
- Concurrency: goroutines + `errgroup` for the per-host refresh fan-out, which
  is the one genuinely concurrent part and where Go shines.
- All `zfs`/`zpool` output parsed by hand from `-H -p` tab-separated output per
  the no-unnecessary-deps preference. Full dep list stays around: bubbletea,
  lipgloss, modernc.org/sqlite, toml, x/sync.

### SSH: shell out to the system `ssh` binary

No ssh library. We exec `ssh [opts] host cmd`, which gives us the user's
`~/.ssh/config`, agent, ProxyJump, ControlMaster multiplexing (we'll set up a
ControlMaster per host for refresh speed) for free. Per-host config can add
`user`, `addr`, `port`, extra options. Agent forwarding (`ssh -A`) is used when
launching a job on the source host so it can open src→dest connections without
provisioned keys; an established connection survives the agent going away
(agent is only needed at handshake), so jobs keep running after the laptop
sleeps — see job protocol below for the failure/resume story.

## Discovery & state

### Refresh (per host, in parallel, single ssh round-trip each)

One ssh invocation per host running a small command batch:

```
zpool list -H -p -o name,size,alloc,free,health,guid
zfs list -H -p -t filesystem,volume -o name,guid,used,avail,refer,creation,createtxg,written,origin,encryption,keystatus,keylocation,mounted,canmount,mountpoint,readonly,receive_resume_token,compression,recordsize
zfs list -H -p -t snapshot -o name,guid,used,creation,createtxg
ls + cat of the zfstui job state dirs (see job protocol)
```

Unreachable hosts (short connect timeout) are marked stale in the UI with the
last-seen time; their cached data is kept and shown dimmed.

### sqlite schema (a pure cache — fully rebuildable from scratch)

```
hosts(name PK, addr, user, last_refresh, reachable)
pools(host, name, guid, size, alloc, free, health)
datasets(id PK, host, pool, name, guid, ...props)
snapshots(dataset_id, name, guid, creation, createtxg, used)
groups(id, display_name)            -- derived, recomputed on refresh
group_members(group_id, dataset_id, state)  -- ahead/in-sync/behind/diverged/clone
jobs(id PK, host, src_dataset, dst_host, dst_dataset, state, from_snap, to_snap,
     bytes_total, bytes_done, started_at, finished_at, log_path)
```

DB at `~/.local/state/zfstui/state.db` by default. `--db` to override,
`--db :memory:` for ephemeral runs with inline inventory.

## Sync planning

Every (source, destination) pair is evaluated **independently** — a group
being diverged doesn't block syncs between its still-compatible replicas.

**Compatibility gate** (must pass before any plan is made). A destination is
compatible with a source iff one of:

- it doesn't exist yet (→ full send, with size confirmation), or
- it carries a resume token whose target snapshot exists on the source
  (→ resume), or
- its newest snapshot's GUID exists on the source **and** the destination is
  clean past that point: no snapshots of its own beyond it and `written == 0`.

Anything else means the receive could only succeed via `recv -F`/rollback,
i.e. destroying data on the destination — rejected with a message naming the
offending snapshots/bytes. The sync dialog shows incompatible destinations
greyed out with that reason rather than hiding them.

Given a compatible (group, source replica, dest replica):

1. Latest source snapshot = max by `creation`.
2. Common base = newest snapshot whose GUID exists on both sides.
3. If dest has `receive_resume_token` → resume: `zfs send -t <token>`.
4. Else incremental `zfs send -I <common> <latest>` (`-I` so dest receives
   intermediate snapshots too; flag to use `-i` instead).
5. Dest doesn't exist → full send (with confirmation, since it can be huge).
   (Other no-common-base / unclean-dest cases never reach planning — the
   compatibility gate already rejected them.)
6. Flags: `-w` (raw) when source `encryption != off`; `-L` always; recv side
   `-s` (resumable) `-u` (don't mount). `-F` is never passed — not even behind
   a confirmation. Mountpoint/canmount on dest are left alone (raw recv
   preserves props; your `bak/` convention handles mounting).
7. Dry-run mode prints/logs the exact pipeline without executing — this exact
   command line is also what goes into the generated job script, so dry-run
   output is trustworthy.

Engine selection per job: `auto` (use syncoid if found on the source host, else
raw ssh+bash), or forced `syncoid` / `raw` via config/flag/dialog. The raw
pipeline inserts `mbuffer`/`pv` when present on the relevant host, with a log
warning when not.

### Data safety (hard rules)

Data safety is the top constraint. The complete list of state-mutating commands
the tool is ever allowed to run on any host:

- `zfs snapshot [-r]`
- `zfs send ... | zfs recv -s -u` — **never `-F`**
- `zfs destroy <dataset>@<snap>[,<snap>...]` — snapshots only, via prune (below)
- `zfs receive -A <dataset>` — abandon a partial/resumable recv, only via an
  explicit user action on a "resumable" cell

Never, under any code path: `zfs destroy` of a filesystem/volume/clone,
`zfs rollback`, `zfs rename`, `recv -F`, `destroy -R`/`-f`, `zpool` mutations,
property changes. If a destination has diverged or otherwise needs surgery,
the tool refuses with a clear explanation and the user fixes it with a shell.
This allowlist is enforced structurally: command lines are built by typed
constructors in one package, and job-script golden tests assert nothing
outside the allowlist ever appears.

## Snapshot pruning

In scope (unlike retention *policy* automation, which stays sanoid's job —
pruning here is interactive/explicit, not a daemon).

- Operates on **snapshots only**, one group at a time. `d` (destroy snapshots)
  opens the prune dialog on the selected group/replica.
- Dialog shows the group's snapshots as rows (columns = replicas, so you see
  which replica has which snapshot), with checkboxes; multi-select what to
  delete and on which replicas. Convenience presets fill the selection
  ("keep last N", "keep only snapshots present on all replicas") but the user
  always sees and confirms the explicit final list.
- **Safety invariant**: for every pair of replicas, their newest common
  snapshot is never deletable (it's the incremental base; deleting it forces a
  full resend). Same for any snapshot that is a clone origin, and for resume-
  token targets. These rows are shown locked with the reason.
- Confirmation screen lists exact `zfs destroy dataset@snap` commands and
  per-snapshot `used` space to be reclaimed. Plain `destroy` only — if zfs
  refuses (e.g. a hold or an unnoticed dependent), that's surfaced as the
  error, never overridden with `-d/-f/-R`.
- Batch: `zfstui prune <group> --keep-last N [--on host] [--dry-run]` with the
  same invariants; `--dry-run` prints the would-be destroy list.

sudo: per-host `sudo = true|false` and optional separate send/recv users
(`send_user`, `recv_user`) since those can differ per host. Commands get
wrapped `sudo zfs ...` as configured. (`zfs allow` delegation is a documented
alternative for avoiding sudo.)

## Remote job protocol (the keystone)

A job = a directory on the **source host** (or the control node itself when
the user overrides "run from"): `~/.local/state/zfstui/jobs/<job-id>/` with:

```
job.sh      generated, self-contained bash (the only thing that "runs")
meta.json   src/dst/snapshots/engine/flags — written by control node
status      one line: running <pid> | done | failed <exitcode>  (+ rfc3339 time)
log         stdout+stderr of the pipeline
progress    pv machine-readable output (bytes done / total), if pv available
```

- Job IDs are `<utc-timestamp>-<short-random>` — sortable, collision-free.
- Launch: control node writes the dir over ssh, then
  `ssh -A host 'cd <dir> && setsid bash job.sh >log 2>&1 & echo $! > pid'`
  and disconnects. The send|recv pipeline is already connected at that point,
  so the agent is no longer needed.
- `job.sh` traps errors, writes `status` on every exit path, and does **no
  retries** in v1: if the network drops mid-transfer it fails fast, recv `-s`
  leaves a resume token on dest, and the next refresh shows a "resumable" cell —
  one keypress relaunches as a resume job. (Simpler and more predictable than
  in-job retry loops; can revisit.)
- Refresh scans each host's jobs dir (recent N, say 50) and upserts into the
  `jobs` table — this is how a fresh control node learns about in-flight or
  finished transfers it never started.
- Old job dirs are pruned by age/count during refresh.

## TUI

Single screen, three regions; `Tab` cycles focus. Modal dialogs on top.

```
┌ Datasets ──────────────────────────────────────────────────────────────┐
│              dev1:ssd       dev2:ssd        dev2:tank     hetzner:tank │
│  git         srv/           bak/@20260601   bak/@20260501              │
│  blobs/01                   srv/            bak/@20260501 bak/@2026…   │
│  blobs/02                  [srv/]           bak/@20260501              │
│  …                                                  (scrolls both axes)│
├ Details: dev2:ssd/srv/blobs/02 ──────────┬ Jobs ──────────────────────┤
│ ahead · encrypted(raw) · 1.2T used       │ ▶ 20260610-a3f2 git→hetzner │
│ latest @20260610  written 3.4G           │   [█████░░░░] 55% 312MB/s   │
│ snapshots: 20260610 20260601 20260501 …  │ ✔ 20260609-9c1b blobs/01…   │
├ Log ─────────────────────────────────────┴────────────────────────────┤
│ 17:21:03 refresh: 4 hosts ok, 0 unreachable (1.8s)                     │
└ q quit · ? help · Tab pane · r/R refresh · s snap · c copy · d prune ──┘
```

Keyboard scheme (mnemonic letters, consistent everywhere):

- `q` quit, `?` help overlay, `Tab`/`Shift-Tab` switch pane
- arrows / `hjkl`: move cursor; table cursor selects a *cell* (replica) or a
  *row* (group, when in the name column); details pane follows the cursor
- `r` refresh hosts relevant to selection, `R` refresh all
- `s` snapshot dialog: name (default today's `YYYYMMDD`), recursive y/n,
  replicas to snap on (default: the ahead replica; the selected cell if the
  group is in sync)
- `c` copy/sync dialog: source (default: the ahead replica; must be chosen
  explicitly if none), destinations (default: all *compatible* replicas of
  the group; incompatible ones greyed out with the reason), engine, recursive,
  dry-run toggle, "run from" (default: source host)
- `d` prune dialog (destroy snapshots) for the selected group/replica
- `h` toggle showing hidden (snapshot-less) groups
- in Jobs pane: `Enter` opens job's log in the Log pane, `x` cancel (kills pid)
- dialogs: `Enter` confirm, `Esc` cancel
- function-key aliases for the F-key inclined: `F5`=copy, `F2`=snapshot

Color: green in-sync, bold ahead, yellow behind, red diverged/error, cyan
resumable, dim unreachable-host columns. Suffix codes carry the same info for
no-color terminals.

## CLI (batch mode)

```
zfstui                                  # TUI
zfstui refresh [--hosts a,b]            # update cache
zfstui status [group...]                # the table, printed plain (cron-able)
zfstui snap <group|dataset> [--name N] [-r]
zfstui sync  <group|--all> [--to host[:pool]...] [--from host] [--engine raw|syncoid]
             [--dry-run] [--detach|--wait]
zfstui prune <group> [--keep-last N] [--on host] [--dry-run]
zfstui jobs [--watch] / zfstui log <job-id>
zfstui doctor                           # per-host tool/sudo/ssh sanity checks
```

`sync --all --dry-run` is the confidence-building command: it prints every
send|recv it *would* run, computed from a fresh refresh.

## Config (TOML)

```toml
# ~/.config/zfstui/config.toml — hostnames are the only required content
[defaults]
engine = "auto"            # auto | raw | syncoid

[hosts.dev1]
[hosts.dev2]
user = "ivan"
sudo = true
[hosts.hetzner]
addr = "h1.example.com"
recv_user = "zfsrecv"
```

`--hosts dev1,dev2` on the command line *adds to / filters* the configured
inventory (ansible-ish); with no config file at all it *is* the inventory.

## Testing strategy

- Parsers (`zfs`/`zpool` output → structs) are pure functions tested against
  canned fixture outputs, including ugly cases (tabs in nothing, `-` values,
  odd names).
- Group/source/freshness derivation and sync planning are pure functions over
  the inventory model: table-driven tests for the scenarios in the README
  (divergence, clones, same-name-unrelated, missing common base, resume
  token), plus the compatibility gate: every unclean/foreign destination must
  be rejected, every clean-ancestor one allowed.
- Job script generation: golden-file tests on the emitted bash; `bash -n` it.
- ssh layer behind an interface so the whole refresh path can run against a
  fake host returning fixtures.
- Command-allowlist test: every mutating command line the codebase can emit
  goes through one constructor package; goldens assert nothing else appears.
- Later: integration harness — a VM/container with a few file-backed zpools
  and ssh-to-localhost simulating multiple "hosts".

## Implementation notes (as of M3, 2026-06-10)

Flat `package main`, one directory (user preference — keep it flat). File
map and the invariants worth knowing before touching code live in
[CLAUDE.md](CLAUDE.md); the short version of the data flow:

```
refresh:  config → ssh (DiscoverCommand, 1 round-trip/host, errgroup×8)
            → ParseDiscovery → store.PutHostState (atomic per host)
view:     store → loadData → Groups (union-find by snapshot GUID; display
            names; pairwise ahead/behind/diverged; hidden rule)
            → statusview (CLI) / tui (bubbletea)
```

Non-obvious implementation decisions, with reasons:

- `store.go` pins sqlite to **one connection** — parallel refresh writers
  otherwise race to SQLITE_BUSY, and `:memory:` would otherwise mean a
  different database per pool connection. Don't "optimize" this away.
- Discovery output sections carry their own exit codes (`===ZFSTUI:rc=N===`)
  so a missing zfs binary / sudo failure on one section is attributable
  instead of silently yielding an empty host.
- The TUI cursor uses negative rows for the header (−2 host, −1 pool); the
  name column is `curCol == 0`, data columns are 1-based into the sorted
  `host:pool` column list. Left/right on the host row jumps host-to-host.
- Cell text logic is shared (`cellText`) between CLI and TUI; the TUI adds
  color on top, the CLI relies on markers alone.
- The "ahead" state intentionally has **no marker/asterisk** in cells: the
  newest `@name` in the row identifies the source (user preference).
- Display-name pipeline: name-in-pool → `trimBoring` (drop leading
  components whose ancestor dataset has zero snapshots) → longest common
  suffix across non-clone members → remainder becomes the cell prefix.
- Everything derived is recomputed on every load; sqlite is a pure cache and
  deleting it is always safe.

Testing: pure-function table tests for parsers/derive (see the `fx` builder
in derive_test.go), a `fakeRunner` end-to-end in main_test.go, sqlite
round-trip in store_test.go. The TUI's rendering can be smoke-checked under
`script`(1) but key delivery there is unreliable — behavior testing of the
TUI is manual for now.

## Roadmap

1. ✅ **M1 — see**: config, ssh runner, parsers, sqlite cache, grouping/source
   derivation, `refresh`/`status` CLI. (Most of the risk and most of the value
   for confidence-building lives here.)
2. **M2 — plan** *(next)*: sync planner + `sync --dry-run`, `doctor`.
3. ✅ **M3 — watch**: read-only TUI (table with two-level host/pool header,
   details pane with Tab focus + scrolling, host/pool/group/cell details,
   async refresh, hidden-group toggle, boring-prefix-trimmed names).
4. **M4 — act**: job protocol, `snap`, real `sync` (raw engine), jobs/log
   panes, resume handling.
5. **M5 — polish**: syncoid engine, prune dialog + `prune` command, job
   cancel, job-dir pruning, packaging.

## Decisions log

- 2026-06-10: language = **Go** (owner familiarity); bubbletea/lipgloss,
  modernc.org/sqlite, BurntSushi/toml, stdlib flag.
- 2026-06-10: snapshot **pruning is in scope** as an interactive dialog +
  batch command (see Snapshot pruning); retention *automation* still isn't.
- 2026-06-10: hierarchical datasets = plain rows, parent dataset gets its own
  row; no collapsing in v1.
- 2026-06-10: **no destructive/rollback operations, ever** — no `recv -F`,
  no dataset destroy, no rollback, even behind confirmations. If a replica is
  messed up beyond a clean incremental, the tool refuses and the user repairs
  it manually. Codified in "Data safety (hard rules)".
- 2026-06-10: **no naming-convention logic** — no backup/master prefix config
  or hardcoding; `srv/`/`bak/` are just one user's setup. Display names are
  derived (longest common path suffix), cells show per-replica remainders.
- 2026-06-10: **master is derived, not declared** — the replica with newest
  unreplicated data is the source; ≥2 such replicas = diverged (flagged for
  manual repair); none = in sync, direction moot. Pins dropped entirely; the
  sqlite cache is now fully rebuildable.
- 2026-06-10: **divergence is pairwise, not group-wide** — syncs between
  compatible pairs of a diverged group are allowed; the compatibility gate
  (dest clean at a snapshot the source has, or absent, or resumable) rejects
  exactly the destructive ones.
- 2026-06-10: **hidden groups** — snapshot-less groups (system/plumbing
  datasets) are hidden by default in status/TUI; `-a` / TUI `h` toggles
  showing them. Purely computed, no persisted flags. Display-only.
- 2026-06-10: **boring prefix trimming** — display names drop leading
  components whose ancestor dataset is snapshot-less plumbing; the prefix
  moves into the cell. Convention-free `bak/`/`srv/` cleanup.
- Name: `zfstui`. Jobs dir: `~/.local/state/zfstui/jobs` (default; works
  non-root).
