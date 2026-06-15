# zfstui design doc

## Status (2026-06-15)

**Working end-to-end on a real fleet**: discovery + status (M1), full TUI
(M3), sync planner with compatibility gate (M2 core), and snapshot/sync
execution with detached jobs and live progress (most of M4). The Details pane
and per-object modals are now unified object views rendered compact (pane) or
full (interactive window stack, Esc-pops); job logs, host uptime/space, and a
pool fill-bar header row were added (see the dated decisions log). Not yet:
batch `snap`/`sync` CLI, prune, jobs pane, `doctor`, job-dir cleanup.

- `zfstui refresh` ssh-es to all known hosts in parallel and scrapes
  `zpool/zfs get -Hp all` (every property + source), disk health (vdev
  properties per pool, verbatim `zpool status`, `lsblk -b -P` physical disk
  inventory, `smartctl -a` per physical disk) plus `/tmp/zfstui-job-*` job
  state into a sqlite cache (a pure cache — safe to delete any time).
  Read-only on the hosts.
- `zfstui status` prints the groups × host:pool matrix. Replication groups
  and per-replica states (ahead/behind/diverged/resumable/clone) are derived
  from **snapshot GUIDs**, never names. Cell markers: `!` diverged,
  `%` clone (resumable carries no marker — cyan color + detail only);
  unsnapshotted writes are indicated by color only
  (no `+` suffix — color is enough). Snapshot-less replicas with writes show
  their written size in parens `(123.4G)` instead of a snapshot name. The
  name column shows the latest-snapshot refer size; for hidden snapshot-less
  groups it shows the largest written (or used) size across members. Snapshot-
  less "plumbing" groups are hidden (`-a` shows) and boring `bak/`/`srv/`-
  style prefixes are auto-trimmed from row names into cells — all
  convention-free, derived from the data.
- bare `./zfstui` opens the TUI: scrollable matrix with a two-level
  host/pool header (both navigable; hosts show "(53%/38T)" capacity when the
  column is wide enough), color-coded cells (stale replicas gray; unhealthy
  pools red), row guide on the cursor's row, async refresh (`r` host / `R`
  all), `h` toggles hidden groups, `?` help. The main table has no title
  line. The Details pane is interactive: segmented title chips
  (`──[ ws:tank/srv/git (/) ]──[ 21.8G used, @20260612 (10.3G) +22K ]──`),
  per-view tables (JOBS + SNAPSHOT for a dataset, POOL for a host, DATASET
  for a pool, REPLICAS for a group); Tab focuses it, ↑↓ move a row cursor,
  and Enter (there or directly on a table cell/header) opens a modal with
  EVERYTHING known about the object — the full `get all` property map with
  sources for datasets/snapshots/pools, access+pools for hosts, meta+command
  for jobs.
- **sync (the core!)**: `y` opens the sync dialog for the selected dataset:
  destinations in its group that need syncing are pre-checked (up-to-date
  skipped; diverged/dirty/clone listed disabled with reasons), intermediate
  snapshots (`-I`) default-on, resume of interrupted receives offered when a
  token is present, plus a free-form editbox for new/custom destinations
  (`host:pool/dataset` → full send; works for seeding a first backup of a
  single-replica dataset). One detached job per destination runs on the source
  host (`zfs send … | mbuffer | pv | ssh dest zfs recv -s -u …`; `-w` raw
  for encrypted sources, `-L -c` for plain ones), using the
  control node's forwarded agent only to authenticate a per-job ssh
  ControlMaster — transfers survive the laptop disconnecting. Running
  transfers show a spinner on the affected cells and live progress
  ("1.2G / 3.4G (29%)", via pv + `zfs send -nP` estimate) in the details
  pane, polled every ~3s; completion auto-refreshes.
- **snapshots**: `s` snapshots the selected dataset via a
  dialog (name, default YYYYMMDD; recursive toggle; live preview of the
  exact `zfs snapshot` command). Runs as a detached job in
  `/tmp/zfstui-job-<id>/` on the host (status/log/meta files; survives
  control-node disconnect), then auto-refreshes. A global `-dry-run` flag
  disables all mutations and shows the would-be command + job script
  instead. All mutation command lines are built in one audited place and
  allowlist-tested.

Try: `go build && ./zfstui refresh root@host1 user@host2 && ./zfstui`.
Hosts are positional specs (`host`, `user@host[:pool]`, or
`ssh://user;send_user=x;recv_user=y@host[:port][/pool]`); the hostname is the
identity, access info is remembered in the sqlite cache (`./zfstui.db` in cwd
during development; disposable), so after the first run a bare `./zfstui`
works. No config file. Non-root access user implies sudo.

Docs: [DESIGN.md](DESIGN.md) — architecture, decisions log, roadmap,
implementation notes; [CLAUDE.md](CLAUDE.md) — code map and conventions for
coding sessions. Code is a flat `package main`, tested ssh-free (parsers,
derivation scenarios, fake-runner end-to-end). Data safety is the #1
constraint: destructive ops (`recv -F`, rollback, dataset destroy) are
banned outright.

Core ideas:
- correlate replicas across hosts by **snapshot GUID**, never by name
- shell out to system `ssh` (user's ssh config/agent/ProxyJump work for free)
- replication runs as a detached, self-describing **job dir** on the source
  host (script+status+log+progress files), so transfers survive control-node
  shutdown and any fresh control node can rediscover them on refresh
- no master role stored, no pins, no name-prefix conventions: the sync source
  is *derived* — the replica holding data the others lack; ≥2 such replicas =
  diverged (flagged red for manual repair); none = in sync, direction moot.
  Divergence is pairwise: compatible pairs of a diverged group still sync;
  only destructive receives are rejected

## One-paragraph summary

A single static binary (`zfstui`) running on a *control node* (your laptop). It
ssh-es into the known hosts (positional specs, remembered in sqlite — no
config file), scrapes `zpool`/`zfs` machine-readable
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

There is no stored "master" role, no pins, no name heuristics. Per-replica
states key off the group's **newest snapshot** (GUID, by `creation`):

- **ahead** — sole holder of the newest snapshot (every peer lacks it) → the
  source; sync defaults flow from it.
- **synced** — holds the newest snapshot (alongside ≥1 peer that also holds it),
  no uncommitted writes; nothing to do.
- **modified** — holds the newest snapshot but has uncommitted local writes
  (`written > 0`). NOT a replication source on its own (uncommitted data can't
  be sent — snapshot it first), and crucially it does **not** make its peers
  stale. This is the common "live master with pending changes, backups already
  caught up to the last snapshot" case (the `dna` example).
- **stale** — missing the group's newest snapshot; cleanly behind, can receive.
- **diverged** — two replicas each hold SNAPSHOTS past their newest common one
  (a real fork) → flagged red. Does *not* disable syncing for the group: the
  sync dialog (or `--from`/`--to`) can still replicate any compatible pair (see
  the compatibility gate in Sync planning); only destructive receives are
  rejected, with the reason shown. Repairing the fork itself stays manual.

Key split (fixed 2026-06-15): **snapshot lineage** decides ahead/synced/stale/
diverged (GUID-only); **uncommitted writes** are a separate axis that only marks
the holder `modified`. Writes NEVER make a replica diverged/ahead or its peers
stale — that produced false divergence from atime on mounted copies. (Snapshot
equality is GUID equality, full stop: `creation` has 1s resolution and
`createtxg` is pool-local — neither may break ties across replicas.)

Wrong-direction replication is structurally impossible: the tool only ever
proposes sending from a replica that provably holds snapshots the dest lacks.

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
| synced    | holds the group's newest snapshot, `written` == 0                 | `bak/@20260601` white |
| ahead     | sole holder of the newest snapshot (peers lack it)                | `srv/@20260610` green |
| modified  | holds the newest snapshot but `written` > 0 (uncommitted writes)  | `srv/@20260610` yellow |
| stale     | missing the group's newest snapshot                               | gray, `@<its-latest>` |
| diverged  | holds snapshots past the common base while another replica does too | red `!` marker |
| resumable | has a `receive_resume_token` (interrupted recv)                   | cyan color (no marker) |
| clone     | `origin` set (clone of another snapshot)                          | dim `%` marker |

### Language: Go (bubbletea + pure-Go sqlite)

Chosen over Rust because the owner is more at home in Go — long-term
hackability of the tool by its user beats marginal library preferences.

- TUI: `bubbletea` + `lipgloss` (+ `bubbles` for table/inputs where they fit).
- sqlite: `modernc.org/sqlite` (pure Go — keeps cross-compilation and the
  single-static-binary story trivial; no cgo).
- CLI: stdlib `flag` with manual subcommand dispatch — no cobra; the
  command surface is small. No config file at all (see Hosts & access).
- Concurrency: goroutines + `errgroup` for the per-host refresh fan-out, which
  is the one genuinely concurrent part and where Go shines.
- All `zfs`/`zpool` output parsed by hand from `-H -p` tab-separated output per
  the no-unnecessary-deps preference. Full dep list stays around: bubbletea,
  bubbles (textinput), lipgloss, modernc.org/sqlite, x/sync.

### SSH: shell out to the system `ssh` binary

No ssh library. We exec `ssh [opts] host cmd`, which gives us the user's
`~/.ssh/config`, agent, ProxyJump, ControlMaster multiplexing (we'll set up a
ControlMaster per host for refresh speed) for free. User/port come from the
host's access spec; aliasing and jump hosts are `~/.ssh/config`'s job.
Agent forwarding (`ssh -A`) is used when
launching a job on the source host so it can open src→dest connections without
provisioned keys; an established connection survives the agent going away
(agent is only needed at handshake), so jobs keep running after the laptop
sleeps — see job protocol below for the failure/resume story.

## Discovery & state

### Refresh (per host, in parallel, single ssh round-trip each)

One ssh invocation per host running a small command batch:

```
zpool get -Hp all
zfs get -Hp -t filesystem,volume all
zfs get -Hp -t snapshot all
glob+dump of /tmp/zfstui-job-* (status, meta.json, progress, expected)
```

`get all` (2026-06-12, replacing the earlier `list -o <columns>`): every
property is collected including its source, so detail views can show
everything and adding a displayed field never touches the protocol or
schema. Typed fields derive from the property maps in one place
(zfsparse.go fromProps); sqlite stores the maps as JSON.

Unreachable hosts (short connect timeout) are marked stale in the UI with the
last-seen time; their cached data is kept and shown dimmed.

### sqlite schema (a pure cache — fully rebuildable from scratch)

```
hosts(name PK, addr, user, last_refresh, reachable)
pools(host, name, guid, size, alloc, free, health)
datasets(id PK, host, pool, name, guid, ...props)
snapshots(dataset_id, name, guid, creation, createtxg, used)
groups(id, display_name)            -- derived, recomputed on refresh
group_members(group_id, dataset_id, state)  -- ahead/synced/stale/modified/diverged/clone
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
6. Flags by source encryption: encrypted → `-w` (raw, required for key-less
   backups); unencrypted → `-L -c` and **never `-w`** — for unencrypted
   datasets `-w` implies `-e` (embedded blocks), and such streams "may not
   be received as an encrypted dataset" (zfs-send(8)) — i.e. it breaks
   unencrypted→encrypted-parent syncs with "inherited key must be loaded"
   (found in the field 2026-06-11). Recv side `-s` (resumable) `-u` (don't
   mount). `-F` is never passed — not even behind a confirmation.
   Full sends of *unencrypted* sources receive with `-o encryption=off`:
   the new child matches the source instead of inheriting the destination
   parent's encryption (which would demand the parent's key be loaded —
   second field bug 2026-06-11). Mountpoint/canmount on dest are left alone
   (raw recv preserves props; your `bak/` convention handles mounting).
7. Dry-run mode prints/logs the exact pipeline without executing — this exact
   command line is also what goes into the generated job script, so dry-run
   output is trustworthy. Implemented as a global `-dry-run` flag: all
   mutations are disabled; confirming a dialog shows the command, the job dir
   and the full job.sh in an overlay instead of executing.

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
constructors in one place (mutate.go: charset validation + shell quoting),
and tests assert both the constructors' output and every mutating line of
generated job scripts match the allowlist regex (mutate_test.go,
jobs_test.go).

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

sudo is implied by the access user: ssh-ing as root runs commands plainly,
any other user gets `sudo -n` wrapping — no separate option. Separate
send/recv users (`send_user`, `recv_user` URL options) exist since those can
differ per host. (`zfs allow` delegation is a documented alternative.)

## Remote job protocol (the keystone)

A job = a directory on the **executing host** (the source host for syncs; or
the control node itself when the user overrides "run from"):
`/tmp/zfstui-job-<job-id>/` — deliberately /tmp rather than a user dir, so we
aren't tied to account layouts and discovery of active/finished jobs is one
glob (`/tmp/zfstui-job-*`) away. Contents:

```
job.sh      generated, self-contained bash (the only thing that "runs")
meta.json   id/kind/host/command + per-kind fields — written by job.sh itself
            (heredoc), so the whole job is a single file transfer
progress    pv -nb byte counts (2s interval) when pv is available
expected    estimated stream size from a no-op `zfs send -nP`
status      one line: running <pid> | done | failed <exitcode>  (+ rfc3339 time)
log         stdout+stderr of the pipeline
progress    pv machine-readable output (bytes done / total), if pv available
```

- Job IDs are 6 random base62 chars (e.g. `kF3x9Q`); time ordering comes
  from meta.json's "created" field, not the ID.
- Launch is ONE ssh round trip: job.sh arrives on the remote command's stdin
  (`mkdir -m 0700 dir && cat > dir/job.sh && setsid bash dir/job.sh ... &`),
  prints `launched <dir>`, and the control node may disconnect. (`ssh -A` for
  syncs: the send|recv pipeline is already connected at that point, so the
  agent is no longer needed afterwards.)
- status file format: `running <pid> <ts>` | `done <ts>` | `failed <rc> <ts>`,
  rewritten on every exit path of job.sh.
- Implemented (M4, 2026-06-11): job mechanics (jobs.go), `snapshot` and
  `sync` job kinds, refresh-time job discovery (4th discovery section), and
  live progress: while running jobs are known, the TUI ticks a spinner
  (500ms) and re-polls the job dirs every ~3s (status/progress only — much
  lighter than a refresh); job completion auto-refreshes affected hosts.
  Receiving/sending cells show the spinner; the details pane shows
  "bytes / total (pct)" per job plus recent finished/failed jobs.
- `job.sh` traps errors, writes `status` on every exit path, and does **no
  retries** in v1: if the network drops mid-transfer it fails fast, recv `-s`
  leaves a resume token on dest, and the next refresh shows a "resumable" cell —
  one keypress relaunches as a resume job. (Simpler and more predictable than
  in-job retry loops; can revisit.)
- Refresh scans each host's jobs dir (recent N, say 50) and upserts into the
  `jobs` table — this is how a fresh control node learns about in-flight or
  finished transfers it never started.
- Old job dirs are pruned by age/count during refresh.

## TUI (as built)

Single screen, a main datasets table plus interactive Details and modal
dialogs/popups composited over the table. The main table has no pane title.
Details keeps segmented pane titles: `──[ id ]──[ info chips ]──`; the
focused pane's first segment is filled white-on-dark-border, including the
padding spaces inside the chip. Modal dialog titles use the same filled
title treatment. Modal bodies use 2-space horizontal padding; the cached
property dump sections render as plain `── properties ─...` headers with
unindented key rows.

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

- `q` quit, `?` help overlay, `Tab` switch pane focus
- arrows / `jk`: move cursor (NOT vim h/l — `h` is the hidden toggle);
  ↑ past the first row selects a pool, ↑ again a host (←→ jump host-to-host)
- `Enter`: detail modal for the selection (dataset cell, host or pool
  header, group name column → group modal); multi-replica cells drill into
  Details instead. In focused Details, ↑↓ move between selectable rows
  (snapshots/jobs/pools/datasets/replicas), Enter opens that object's modal
- `r` refresh the selected host, `R` refresh all (async; spinner + live
  progress polling while jobs run)
- `s` snapshot dialog: name input (default `YYYYMMDD`), recursive checkbox,
  live command preview; Create acts immediately (no confirm screen)
- `y` sync dialog: member checkboxes pre-checked iff they need syncing,
  disabled rows show reasons, free-form "new dest" editbox (full send),
  `-I` intermediate default-on
- `h` toggle showing hidden (snapshot-less) groups
- dialogs: Tab/↑↓ cycle focus, Space toggles, Enter = default action, Esc
  cancel; in -dry-run, action buttons say "(dry run)" and show a scrollable
  plan modal instead of executing

Colors: white normal, gray behind/stale, green unsynced snapshots (ahead),
bright yellow unsnapshotted writes (`+`), red diverged and unhealthy/error
pool headers, cyan resumable + section headers + status messages, dim
clone/unreachable. Markers carry the same info without color. Planned, not
yet built: `d` prune dialog, a jobs pane, `x` job cancel.

## CLI (batch mode)

```
zfstui [host-spec...]                   # TUI (specs limit + override access)
zfstui refresh [host-spec...]           # update cache
zfstui status [host-spec...]            # the table, printed plain (cron-able)
zfstui snap <group|dataset> [--name N] [-r]
zfstui sync  <group|--all> [--to host[:pool]...] [--from host] [--engine raw|syncoid]
             [--dry-run] [--detach|--wait]
zfstui prune <group> [--keep-last N] [--on host] [--dry-run]
zfstui jobs [--watch] / zfstui log <job-id>
zfstui doctor                           # per-host tool/sudo/ssh sanity checks
```

`sync --all --dry-run` is the confidence-building command: it prints every
send|recv it *would* run, computed from a fresh refresh.

## Hosts & access specs (no config file)

The **hostname is the sole host identity** — db key, table column, dedup key.
`root@ws` and `ws` are the same host. Everything else is *access spec*:

```
host                                          # root@host implied
[user@]host[:pool]                            # scp-style; path = zfs pool
ssh://[user[;opt=val...]@]host[:port][/pool]  # chrome-style userinfo options
```

Recognized options: `send_user=NAME`, `recv_user=NAME`. sudo is implied:
non-root access user ⇒ `sudo -n`. The pool path component is parsed/stored/
displayed but not yet enforced as a filter.

There is **no inventory/config file**. Hosts are given as positional CLI
args and remembered (parsed/normalized fields, not raw URLs) in the sqlite
hosts table:

- no positional specs → use all remembered hosts;
- specs given → the command is *limited* to those hosts, and each spec
  *replaces* the host's stored access spec wholesale;
- two different access specs for the same host in one invocation → error;
- the details pane reassembles the stored fields back into a display URL.

The sqlite db is disposable: cache + remembered hosts, never critical (a
schema-version bump simply drops and recreates it).

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

## Implementation notes (as of 2026-06-12)

Flat `package main`, one directory (user preference — keep it flat). File
map and the invariants worth knowing before touching code live in
[CLAUDE.md](CLAUDE.md); the short version of the data flow:

```
refresh:  host specs/sqlite → ssh (DiscoverCommand, 1 round-trip/host, errgroup×8)
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
- Cell text logic is shared (`CellText`) between CLI and TUI; the TUI adds
  color on top, the CLI relies on markers alone.
- Discovery is `get -Hp all`: typed struct fields derive from the property
  maps in one place (zfsparse.go fromProps/toProps); sqlite stores the maps
  as JSON; detail modals dump them fully, with property sources.
- Object views are unified (view.go): one builder per object type emits
  `viewLine`s rendered compact (Details pane) or full (interactive window
  stack) from a single `full bool`; selectable lines carry an `open` closure
  returning the drilled-into object's `*objView`. Scrolling pulls up to 3
  non-selectable context lines (section headers) into view.
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
2. **M2 — plan** *(planner done 2026-06-11 — planSync in sync.go with the
   compatibility gate; batch `sync --dry-run` CLI + `doctor` still open)*.
3. ✅ **M3 — watch**: read-only TUI (table with two-level host/pool header,
   details pane with Tab focus + scrolling, host/pool/group/cell details,
   async refresh, hidden-group toggle, boring-prefix-trimmed names).
4. **M4 — act** *(mostly done 2026-06-11/12: job protocol, TUI snapshot &
   sync dialogs, -dry-run, full sends to custom destinations, resume via
   tokens, refresh-time job discovery, live progress with spinner +
   bytes/percent)*. Still open: batch `snap`/`sync` CLI, a jobs/log pane.
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
- 2026-06-11: **hostname = identity; access is separate** — `root@ws` and
  `ws` are the same host. Specs are positional CLI args (`-hosts` flag and
  the whole TOML config file are gone); ssh/scp URL forms incl. chrome-style
  `;opt=value` options for send/recv users; specs are remembered in sqlite
  (normalized fields) and replaced wholesale by new specs; conflicting specs
  in one invocation are rejected; root is the default user.
- 2026-06-11: **sudo is implied** — root access user ⇒ no sudo, non-root ⇒
  `sudo -n`. No sudo option anywhere.
- 2026-06-11: **jobs live in `/tmp/zfstui-job-<id>/`** (not user dirs):
  account-independent, and active/finished job discovery is a single glob.
  Job launch is one ssh round trip with job.sh on stdin; meta.json is written
  by job.sh itself. First job kind: snapshot (TUI `s` dialog).
- 2026-06-11: **global `-dry-run` flag** — disables all mutations everywhere;
  confirmations show the exact command + job dir + job.sh instead.
- 2026-06-11: **sync implemented (TUI `y`)** — dialog pre-checks exactly
  the destinations that need syncing (skips up-to-date; lists diverged/
  dirty/clone/empty disabled with reasons); `-I` intermediate snapshots
  default-on; one detached job per destination on the source host. Always
  `send -w`; base snapshot resolved via GUID to its source-side NAME (names
  may differ across replicas). Agent story: launch with `ssh -A`, job
  authenticates its own ControlMaster to the destination, touches `ssh-ok`,
  launcher disconnects; reconnects later would fail fast and leave a resume
  token (next `y` offers resume). TRAP (field bug 2026-06-12): the agent-
  forwarded launch must bypass the control node's ControlMaster
  (`-o ControlPath=none` BEFORE the multiplexing options — ssh honors the
  first occurrence) — a reused master silently ignores `-A`.
- 2026-06-11: **custom destinations** — the sync dialog has a free-form
  editbox (`host:pool/dataset`, comma-separated) next to the member
  checkboxes; these get a FULL `send -w` of the source's latest snapshot and
  must be new datasets (existing ones are rejected — receiving into an
  existing unrelated dataset can't succeed non-destructively). Works on
  single-replica groups too (that's how a first backup is seeded).
- 2026-06-11: **progress display** — sync jobs estimate stream size up front
  (`zfs send -nP`, read-only) into ./expected and tick ./progress via
  optional pv; TUI spinner + percentage as above. Old job dirs are not yet
  pruned (manual `rm -rf /tmp/zfstui-job-*` of finished ones is safe).
- 2026-06-12: **disk health collection** — per pool: vdev properties
  (`zpool get all <pool> all-vdevs`, OpenZFS ≥ 2.2; gracefully absent on
  older zfs) for leaf vdev error counters, PLUS verbatim `zpool status`
  for humans; `lsblk -b -P` for physical disk identity and `smartctl -a`
  (SATA+NVMe) per physical disk. Pool details show leaf vdev rows (red on
  errors) + scan/errors lines; pool modals show zpool status; host details
  show physical disks only; disk modals own SMART verbatims. Intentional
  model split: a ZFS vdev row is a
  pool member/path/partition with IO error counters, while a Disk is a
  physical host device with model/serial/WWN/SMART. Do not try to rebuild
  hierarchy in the details pane; `zpool status` is the topology display.
  Current caveats: zvol pseudo-disks (`zdN`) are filtered; vdev→disk mapping
  knows lsblk path/name and by-partlabel aliases, but by-id/by-path pools may
  need explicit alias collection later. SMART reports that cannot be mapped
  to a Disk are intentionally dropped during prototyping rather than shown as
  fallback pseudo-disks.
- 2026-06-12: **discovery via `get all`** — every property + source is
  collected and cached (JSON maps in sqlite, schema v4); typed fields derive
  from the maps in one place. Detail modals show the full dumps.
- 2026-06-12: **interactive Details pane** — segmented title chips, per-view
  tables with a row cursor, Enter-opens-modal everywhere (incl. directly
  from table cells/headers). State display renamed "in-sync" → "synced".
  Docs merged: README.md folded into DESIGN.md (user edit).
- 2026-06-14: **UI polish** — modal titles prefixed with object type
  ("dataset", "snapshot" — host/pool/disk/job/vdev already had it); footer
  swapped so status message is on the left and keyboard help on the right;
  jobs table in Details shows status (OK/N%/FAIL/spinner) before the ID;
  running job progress adds average transfer speed and ETA (from start
  timestamp in status file); PgUp/PgDn works in both main pane (page of
  rows) and Details pane (page of lines). `+` suffix dropped from cells with
  unsnapshotted writes — color alone signals the state. Snapshot-less
  replicas with writes now show `(size)` in the cell; name column shows a
  size estimate for hidden snapshot-less groups (max Written, else Used).
- 2026-06-14: **table header redesign** — host header row drops Bold+Underline
  (just Bold now); host labels are centered within their pool-column span;
  capacity info drops parens ("83%/1.8T" not "(83%/1.8T)"); vertical `│`
  separators between host groups (and after the name column) give a clean
  column-group boundary; top-left name-column header shows "group"; data rows
  show latest-snapshot refer size right-aligned in the name column (integer
  shorthand: "19G", "239M"); when the main pane is focused and the cursor is
  on a data row, the name-column header ("group") or the pool headers of the
  cursor's host group get the active-pane background highlight (same as the
  pane title chip) — gives a column-indicator effect without covering data.
- 2026-06-15: **header live indicators** — host header row shows a braille
  spinner next to the host name while that host is being refreshed (ticker
  also runs during refresh, not just during running jobs); pool header row
  shows 🧹 next to the pool name when a scrub or resilver is in progress
  (detected from `scan: … in progress` in the verbatim `_zpool-status` prop).
  Column width accounts for the emoji so data cells below stay aligned.
- 2026-06-15: **unified object views + window stack** — the Details pane and
  the per-object modals were two separate code paths displaying the same
  objects (host/pool/dataset/snapshot/disk/vdev/job/group). Merged into ONE
  builder per object type (view.go) returning structured `viewLine`s, rendered
  two ways from a single `full bool`: compact (Details pane — no section
  headers, property dumps / verbatim status / SMART dropped, dense `* HEADER` /
  indented-row tables) and full (interactive window — cyan centered `── title ──`
  section headers, complete property maps + verbatim text: the old text-modal
  look, now with a row cursor and Enter-to-drill). Modals became a **window
  stack** (`Model.windows`): Enter on a selectable row pushes a sub-window for
  that object, Esc pops it, focus returns to the previous window. Builders are
  re-run every render, so spinners / transfer progress / ETA stay live in
  windows too (previously only the pane updated; text modals were a static
  snapshot taken at open time). Dry-run plans and failure logs are plain
  windows (`textView`) on the same stack. details.go is now just the pane glue
  (compact build + 8-line render + row cursor); view.go owns all object
  content and the window machinery. Table data rows render white; table
  headers cyan (centered `── title ──` in windows, `* HEADER` in the pane).
- 2026-06-15: **job logs in the view** — `jobsProbe` now also tails `./log`
  (last 200 lines) so `JobInfo.Log` is populated on every refresh and live
  poll; persisted in the jobs table (schema v6). The job window shows it as a
  `── log ──` section after `── script ──`, updating live while a transfer
  runs. (waitJob still tails the log transiently for the immediate
  snapshot/sync failure modals.)
- 2026-06-15: **pool fill bars** — a third, non-focusable header row under each
  pool header shows utilisation as a `▰▰▰▰▱▱` bar (border/dim colored), sized to
  the pool column width and rounded to the nearest cell. The host header row
  drops the `NN%/` prefix (the bars carry utilisation now) and shows just the
  total pool size. tableView now emits THREE header lines (host/pool/bar); the
  data-row count and scrollToCursor account for the extra line.
- 2026-06-15: **host boot time / uptime + space** — discovery gained a
  non-fatal `===ZFSTUI:host===` section (`awk '/^btime/{print $2}' /proc/stat`)
  collecting the host's boot epoch into `HostState.BootTime`, persisted in the
  hosts table (`boot_time`, schema v7) and surfaced as `HostInfo.BootTime`. The
  host window now shows `space:` (total/free/%used across pools) and `uptime:`
  (booted timestamp) rows at the top. The section is tolerated absent (old
  caches / non-Linux) so it never aborts a refresh.
- 2026-06-15: **state taxonomy split — `modified` added; writes ≠ behind** —
  states now key off the group's newest snapshot (GUID): ahead (sole holder),
  synced (holds it, clean), stale (missing it), and the NEW **modified** (holds
  it but has uncommitted writes). Snapshot lineage and uncommitted writes are
  separate axes — `written>0` only marks the holder `modified`, never makes
  peers stale or anyone diverged (kills the atime false-divergence). Renamed
  values: in-sync→`synced`, behind→`stale` (constants StateInSync/StateBehind
  kept). classify() rewritten: pairwise GUID compare for divergence only, then
  per-replica group-newest logic. Colors: green ahead, yellow modified, gray
  stale, white synced. The `dna` fleet example (one live master with +680K
  uncommitted, three backups at the same snapshot) now reads master=modified,
  backups=synced — not "behind".
- 2026-06-15: **pool window top = clickable `zpool status`** — the pool
  window's top section IS the verbatim `zpool status` (no label): vdev lines
  (first token matches a Pool VDev name) are focusable rows opening the vdev
  window (reddened on error), scan:/errors: lines redden on trouble, and a
  `space: <total/used/free>, N% frag, X dedup` line is appended at the end —
  replacing the separate VDEV table and the health/scan/space kv rows. Falls
  back to those kv rows + a VDEV table only when `_zpool-status` is absent.
  pool/host detail chips + window space rows share a `spaceSummary` helper
  ("1.9G total, 315M (16%) used, 1.6G free").
- 2026-06-15: **block labels left-aligned cyan, not centered; kv blocks
  trimmed; host view reshaped** — in full windows labels are LEFT-ALIGNED cyan:
  tables self-label via their cyan column header (DISK/VDEV/POOL/…), property
  dumps via PROPERTY/VALUE, `JOBS` and verbatim blocks (`zpool status`,
  `smartctl -a` via appendVerbatim) get a plain cyan label line. The only
  remaining centered `── title ──` (vsection) headers are the job window's
  `script`/`log`. Each block self-spaces with a leading blank
  (vtable lost its `title` param, propsDump lost its title, the
  layoutSections/withSectionGaps pass is gone — done explicitly per user, no
  redundancy-matching heuristic). Top kv blocks trimmed to essentials (identity
  is in the window title; details are in the props dump): dataset =
  mountpoint/space/created; snapshot = created; vdev = state/errors/disk/path/
  space; disk = size/smart. Host view: chip uses the window's
  `X total, Y free (N% used)` form; pools table moved to the TOP (compact pane:
  POOLS + DISK; full: pools/disk/vdev/dataset then a jobs table last).
- 2026-06-15: **view polish (user-driven tweaks)** — table data
  rows render white, headers cyan; the compact pane dropped ALL `*` bullets
  and indentation (flush, dense — user preference); propsDump omits the SOURCE
  column when the map has no sources (lsblk); window titles are host-qualified
  (`disk h:/dev/sda`, `vdev h:pool/x`, `snapshot h:ds@s`, `job h:id`); the
  dataset window drops host/type rows (host is in the title) and shows
  `encryption … root: <encryptionroot>` only when encrypted, with explicit "0"
  for zero space values; the disk window top shows only size + smart, with the
  `smartctl -a` verbatim moved LAST (after lsblk props). PgUp/PgDn snap the
  cursor onto a visible selectable row (scrollMove) so it never strands behind
  a prior page; up/down free-scroll past the last parkable row to reach
  trailing content.
- 2026-06-15: **control bytes stripped from log/verbatim** — job logs (and
  smartctl/zpool verbatims) often carry carriage returns and ANSI escapes from
  pv/ssh/zfs progress output; a bare `\r` is zero-width to lipgloss but resets
  the cursor on a real terminal, shoving the window border mid-line. `stripCtrl`
  drops C0 control bytes (and DEL/ESC) while keeping tabs for expandTabs;
  applied to job-log and appendVerbatim lines, NOT to propsDump (which embeds
  intentional ANSI).
- 2026-06-15: **jobsProbe is collision-proof** — replaced the multi-marker
  echo soup (`==J==`/`==M==`/`==P==`/`==E==`/`==L==`) with ONE line per job:
  `<dir> status=<b64> meta=<b64> progress=<b64> expected=<b64> log=<b64>`
  (`base64 | tr -d '\n'` per value). base64 has no spaces/newlines so a value
  can't split a token or alias the framing, and the key is always present (an
  empty value is just `log=`). ParseJobs anchors on the dir prefix, then
  splits key=value tokens and base64-decodes.
- Name: `zfstui`.

## Initial wishes

Original user's wishes for this project verbatim.

I have a bunch of linux hosts (laptops, desktops, NAS, cloud servers) with ZFS, and many ZFS datasets replicated across them. I want a tool/system to manage these datasets. Currently I run periodically a bunch of manual `sudo zfs send ... | ssh ... zfs recv ...` commands and want something better, less error prone and easier.

Note on data organization i typically have (keep in mind: this is my typical setup, but not always the case, and also I want tool to be somewhat general also for other people):
  * YYYYMMDD snapshot names as a rule (but not always, so probably sort on internal timestamp of when snapshot was taken to find latest)
  * For each dataset normally only one copy will be master, and othersare backups. note i might have master on ssd pool with backup on hdd on same host even.
  * Usually a replicated dataset's name is the same across all machines, but it may have varying prefixes. Typically (but not always) I like using `bak/` and `srv/` prefix in dataset names (e.g. `dev1:ssd/srv/git` is a master in the master table: `srv/git` dataset on `ssd` pool on host `dev1`). With `srv` automounting datasets in `/srv` dir and `bak` disabling the mount - this make it easy to switch master/backups by merely renaming stuff. Some datasets using hierarchical names e.g `blobs/NN`. (perhaps table needs to have also `blobs` parent entry in these cases? unsure)
  * I also have some datasets that are not replicated despite same name (e.g. `rpool`, `cache` etc) - so you'd need to use actually zfs's internal dataset identifiers to figure out inter-related/syncable datasets. Names are mostly relevant for displaying stuff in shorthand form like in table above when they conform to expectations.
  * some nodes have extra snapshots and clones of snapshots and even divergent datasets, so you should be able to handle/tolerate them, but these are rather exceptional, so avoid overcomplicating stuff for them.
  * usually using encrypted datasets with `zfs send --raw -L` when syncing. pay attention to properies of zfs datasets to make sure zfs send will work and set correct flags for it.

Wishlist/preferences/thoughts below.

* Global/centeralized view/config/tool. I'd prefer to avoid having to setup each host/pair individually and instead having a centralized config or tool or something (e.g. i could kick off sync jobs off of my laptop which has ssh to all hosts)
* Global status
  * control node should know or quickly update/boostrap its knowledge of state of the whole system by sshing to remote hosts and running commands (zfs/zpool etc)
  * e.g. i may have to start the tool on a new laptop, just give it the list of hostnames and it should be able to ssh and figure out what datasets are there, where are they replecated, where's the masters, where are backups and at what snapshot etc.
  * if i add a new host, likewsie, it should quickly figure out its relationship to the rest of hosts and update its knowledge
  * i suggest using sqlite to keep this local state on control node and keep it simple. when ssh'ign to query nodes, probably use zfs/zpool to get machine-readable info about pools and datasets, as much as possible (size of metadata is no concern - we have just O(10-100) hosts and datasets) and store that in sqlite
* Intuitive CLI TUI to view the state and manually kick off replication using keyboard shortcuts or menu
* TUI should have multiple windows i suppose
* One window to display a table with hosts / datasets, maybe something like
  ```
             dev1:ssd     dev2:ssd      dev2:tank     hetzner:tank
    git      srv/         bak/@20260601 bak/@20260501
    blobs/01                            srv/          bak/@20260501
    blobs/02                            srv/          bak/@20260501
  ```
* In the above table I mean i have `dev2:ssd/srv/git` (master), `dev2:ssd/bak/git@20260602` (latest) and `dev2:tank/bak/git@20260501`;
  `dev2:tank/srv/blobs/01` (master), `hetzner:tank/blobs/01@20260501` (backup) etc.
* Arrow keys could be used to move cursor in the table. Perhaps another window could display detailed state of the selected cell
* Color coding, maybe suffix coding ok (e.g. `@<snapshot name>` with no special color indicates its at that latest globally-consistent snapshot and hasn't diverged/no writes after it)
* Should scale to dozens/hundreds of hosts and datasets, so table needs to be scrollable
* Shortcuts to refresh, take a new snapshot (maybe S or F2?), replicate (maybe R or S(sync) or C(copy) or F5(copy))? think of good consistent shortcuts schema. these should pop up a dialog to confirm and set options. snapshot dialog needs to ask snapshot name (default to YYYYMMDD). sync needs to ask to which destinations to replicate (default all current backup destinations for that dataset), and allow specifying new dests or something, recursive copy or not etc.
* should be possible to display log of replication jobs and scroll them? maybe tab to swtich between windows
* window to see progress bars of all transfers; maybe moving cursor there -> show log of a transfer in log window?
* I kick off sync at the control node, but the syncing probably should be done autonomously by the job on the host that has the done. i should e able to shutdown my laptop with control node and syncing should just continue, with some tolerance for network failures. thus, the control node needs to send a batch script over ssh to the host that going to do the syncing - by default the host with master (but allow sync dialog to override, including also using control node itself to do syncing - read from master, write to destinations)
* Global status: should be aware of the replications in the progress that were kicked off and in progress. if control node's state is reset (or i start on another fresh laptop etc), when updating, it should besides zfs/zpool info also fetch status of ongoing replication jobs on remote hosts. so make sure to design how the replication batch jobs run, where they write progress/logs/results/completion status - so that when updating the state, you could pick that stuff up from remote host.
* Language if implementing stuff from scratch: pick rust or golang (static typing to catch errors important, maturity of libraries, your coding expertise are the main factors). maaaybe python if TUI libraries story are compelling and it would make stuff easier but my gut feeling unlikely.
* keep stuff as simple as possible, prefer to minimize unnecessary deps. e.g. parse zfs/zpool command output yourself rather some library.
* besides TUI, also a batch mode is useful. e.g. i should be able to run a command to kick off replication of a specific or all master datasets to backup destinations.
* incremental sends are critical
* inventory config: yaml or toml
  * maybe toml makes more sense since syncoid uses it?
  * normally, just hostname is the single critical piece of info for each host i think, we can probably zeroconfig everything from it right? but sometimes i may need to tweak stuff, like username and ssh key
  * also allow no-config-file by taking inventory from command line flag (like ansible?). database unclear - either ephemeral or the default one but with subset of hosts and add any missing i.e. inline inventory could add, rather than replace hosts?
  * maybe ansible-style investory and vars? then the very basic config could be just `[hosts]` sections and list of hostnames, very simple
  * UPDATE (2026-06-11), superseding the above — host model notes:
    * hostname is the *primary identifier* of a host; `root@ws` and `ws` are the same host (we had a bug where they weren't - never again!)
    * things like `root@ws` or ssh/scp urls are *access specification*; extract the hostname out of them and key everything on it
    * default to `root@` when no username given; non-root username implies sudo (no separate sudo option needed)
    * no inventory/config file and no `-hosts` flag: hosts are positional args, e.g. `zfstui localhost user@dev1 user2@server`
    * no positional args -> take hosts from sqlite (it remembers hosts i'm working with); positional args *limit* the run to those hosts and their access spec *overrides/replaces* what's stored
    * reject if one invocation gives multiple different access urls for the same host (e.g. `root@ws user@ws`)
    * support `ssh://[user[;option=value]@]host[:port][/path]` (chrome-style options carry extra params like send/recv users) and scp-style `[user@]host[:path]`; the path component refers to a zfs pool (for now)
    * sqlite stores parsed/normalized fields (not raw urls); details window reassembles them into a url for display
    * sqlite db is mostly disposable: cache + remembered hosts, never critical to keep
* some hosts may be unreachable, ping when refreshing
* synchronization:
  * i'm ok with using syncoid/sanoid as the lower-level synchronization tool or just raw ssh + bash or both options (sync dialog or config or flag could specify preference, maybe default to autodetecting syncoid/sanoid)
  * when using bash, autodetect the tools needed to speed stuff up (mbuffer, pv etc) but cope gracefully if unavail with warning message in logs
  * should support ssh'ing as root or a user with sudo
  * note: recv and send sudo users may sometimes be different, on a per-host basis
* should be maybe possible to use control node's ssh agent (ssh -A or something) to estable src -> destination ssh connection then disconnect so that I could avoid provisioning ssh keys in typical case. if conn aborted and agent needed but unreachable? well next time i could see it in UI and resume; zfs resume tokens?
* dry mode (both TUI and batch) to help me debug and gain confidence in the tool
* should be properly unittested (without reliance on zfs and syncoid) and later also intergration tests (maybe running on a local VM proeprly set up with zfs and multiple pools to do it or something)
* working names ideas for the tool: zfstui, zfsman, syncoid-tui (to emphasise syncoid? maybe we'll market it as that but it should still supported general ssh/bash fallack)
* not all 100% of above have to be implemented. this is a starting point of roughly what i'd like in a tool. deviation possible, especially for consistency, practicality, simplicity, rationality, common sense welcome.
* make sure to edit this file often enough as you work (add design, notes, project status etc). Frequent update more important, don't keep me in the dark! You can always keep refining your thoughts here later.
