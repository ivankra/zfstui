# zfstui — agent notes

TUI/CLI for managing ZFS dataset replication across ssh-reachable hosts.
READ DESIGN.md first: it is THE doc now (status, architecture, dated
decisions log explaining *why*, roadmap, and the user's original wishes at
the bottom — README.md was merged into it and deleted).

State as of 2026-06-15: discovery/status (M1), TUI (M3), sync planner (M2
core), and most of M4 (snapshot + sync dialogs, detached jobs, live progress,
-dry-run) are DONE and field-tested on the user's real fleet. The Details
pane + per-object modals were unified into ONE builder-per-object set of views
rendered compact (pane) or full (interactive window stack) — see view.go /
details.go and the tui.go bullet. Job logs, host boot/uptime + pool space, and
pool fill-bar header row were added. Missing: batch snap/sync CLI, jobs pane,
prune, job-dir cleanup, doctor.

## Commands

```
go build -o zfstui . && go vet ./... && go test ./...   # all must stay green
gofmt -l .                                              # must print nothing
./zfstui refresh root@ws ivan@cml                       # real ssh discovery (read-only on hosts)
./zfstui status [-a] [host...]                          # matrix from cache
./zfstui [-dry-run] [host-spec...]                      # TUI
```

Host model ("never again" bug class): the **hostname is the sole identity** —
`root@ws` and `ws` are the same host. Specs are positional args (`host`,
`user@host[:pool]`, `ssh://user;send_user=x;recv_user=y@host[:port][/pool]` —
chrome-style `;opt=value` userinfo options). Parsed/normalized access fields
are remembered in the sqlite hosts table and replaced wholesale by new specs;
no specs → use remembered hosts; specs → limit the run to them. Conflicting
specs for one host in one invocation → error. Default user is root;
**non-root user implies sudo** (no sudo option exists). There is **no config
file**. The pool path part is parsed/stored/displayed, not yet enforced.

Dev-mode default (deliberate, revert to XDG before release): sqlite cache
`./zfstui.db`, cwd-relative so the user can run and the agent can inspect the
same file. Disposable: a schemaVersion bump in store.go wipes and recreates
it — no migrations, ever. The user's real fleet db may be present in cwd;
`./zfstui status` works offline from it. Never ssh to the user's hosts
yourself; ask the user to run things and paste output.

## Hard rules (data safety — the project's #1 constraint)

Only ever emit: `zfs snapshot`, `zfs send | zfs recv -s -u` (NEVER -F),
`zfs destroy <ds>@<snap>` (prune only, not implemented yet), `zfs receive -A`
(explicit abandon). Never: dataset/clone destroy, rollback, rename,
`destroy -R/-f`, zpool or property mutations (sole exception:
`recv -o encryption=off`, see mutate.go). ALL mutating command lines are
built in mutate.go and nowhere else (charset validation + shell quoting);
mutate_test.go's allowlist regexes + jobs_test.go's job-script scanner verify
every emitted shape — extend regexes and constructors together. The global
`-dry-run` flag must keep disabling every mutation path.

## Field-bug traps (each cost a real debugging session — don't reintroduce)

1. **createtxg is pool-local.** A received snapshot gets the txg of the recv.
   Snapshot identity/comparison across replicas is GUID-only; never break
   ties with createtxg (made fresh backups look "ahead" of their source).
   Regression: TestReceivedReplicaNotAheadDespiteLargerTXG.
2. **A reused ssh ControlMaster silently ignores -A.** Agent-forwarded job
   launches pass `-A -o ControlPath=none`, and SSH.Args puts caller extras
   FIRST because ssh honors the first occurrence of an option.
   Regression: TestSSHArgsExtraFirst.
3. **`send -w` on an unencrypted dataset implies -e (embedded blocks)**,
   which cannot be received into an encrypted destination. Flags: encrypted
   source → `-w`; unencrypted → `-L -c`, never -w.
4. **Receiving a NEW dataset under an encrypted parent demands the parent's
   key be loaded** — full sends of unencrypted sources therefore recv with
   `-o encryption=off` (child matches source; raw sends carry their own keys
   and don't need this).

## Code map (flat package main, one dir — user explicitly wants no subdirs)

- `model.go` — types: Host (identity=Name + access spec; EffUser/NeedSudo/
  Dest/URL helpers), Pool, Dataset, Snapshot, VDev (leaf/topology objects
  from vdev properties, error counters, optional DiskID), Disk (physical
  disk identity + lsblk props + verbatim smartctl), JobInfo (incl. `Log` =
  tail of ./log), HostState (per-host raw inventory incl. VDevs/Disks/Jobs
  + `BootTime` unix-secs from /proc/stat btime), Replica/Group (derived).
- `hostspec.go` — parseHostSpec/parseHostSpecs (URL forms, conflict
  rejection, root normalized to "") and resolveHosts (specs ↔ sqlite memory).
- `zfsparse.go` — discovery uses `zpool/zfs get -Hp all` (NAME\tPROP\tVALUE
  \tSOURCE lines): EVERY property is collected; typed struct fields derive
  from the prop maps in ONE place (datasetFromProps & friends, with toProps
  inverses for code-built fixtures). DiscoverCommand = ONE ssh round-trip,
  marker-delimited sections with per-section exit codes: pools, disks
  (per pool: `zpool get all <pool> all-vdevs` vdev properties [OpenZFS≥2.2,
  tolerated absent — verbatim status still surfaces] + verbatim
  `zpool status`; then `lsblk -b -P` physical disks and `smartctl -a` per
  disk, 20K cap),
  datasets, snapshots, jobs (`jobsProbe`, also used for live polling), host
  (`===ZFSTUI:host===`: `awk '/^btime/' /proc/stat` boot epoch → BootTime;
  NON-required + `; true` so it never aborts a refresh on non-Linux/old
  caches — parse only if the section is present).
  Verbatim zpool status rides on Pool.Props["_zpool-status"] (underscore =
  synthetic key, excluded from propsDump, persisted for free).
  ParseDiscovery/ParseDisks/ParseJobs. `jobsProbe` emits ONE line per job dir:
  `<dir> status=<b64> meta=<b64> progress=<b64> expected=<b64> log=<b64>` —
  base64 values (no spaces/newlines) so arbitrary log/meta can't alias the
  framing; the key is always present (empty = `log=`). NEVER go back to the
  old `==J==`/`==M==` marker soup — log content aliased the markers.
  Mental model: ZFS leaf vdevs are
  pool membership/error-counter objects, often partitions or by-partlabel
  paths; Disks are host-level physical devices with stable identity (WWN,
  serial, else dev), model/size/transport/media, and SMART text.
- `ssh.go` — Runner interface (Run/RunInput; everything testable offline via
  fakes); shells out to system `ssh` (BatchMode, ConnectTimeout,
  ControlMaster sockets in ~/.local/state/zfstui/cm). Never use an ssh lib.
- `store.go` — sqlite cache (modernc.org/sqlite). Inventory rows store the
  full property maps as JSON (props+srcs); typed fields are re-derived on
  load via the same fromProps mappings. `db.SetMaxOpenConns(1)` is
  load-bearing (SQLITE_BUSY under parallel refresh; `:memory:` per-conn DBs).
  schemaVersion (currently 7) mismatch drops ALL tables — disposable cache,
  no migrations ever. hosts table carries `boot_time`; jobs table carries
  `log`. TRAP: adding a column mid-development can leave existing rows with
  NULL in it (a build wrote rows before the INSERT listed the column) →
  `Scan ... converting NULL to string` on load. Read with `COALESCE(col,'')`
  (see the jobs `log` SELECT); new writes always supply a value.
- `derive.go` — THE core; pure functions of inventory:
  - groups = union-find over shared **snapshot GUIDs** (+ clone `origin`
    links). Dataset GUIDs differ per replica — never identity.
  - display name = longest common path suffix (pool stripped), clones
    excluded; leading components whose ancestor dataset is snapshot-less
    plumbing get trimmed (`trimBoring`); the remainder is the cell prefix.
  - states via pairwise compare against the newest shared snapshot (GUID
    equality only — trap #1): ahead / behind / diverged (both sides ahead) /
    in-sync; written>0 counts as ahead; clone/resume-token override display.
    No stored "master", no pins — the source is always derived.
  - Group.Hidden = no member has any snapshot (plumbing datasets).
- `mutate.go` — THE ONLY place mutation command lines are built:
  buildSnapshotCmd, buildSendCmd / buildFullSendCmd (flags per trap #3),
  buildResumeSendCmd (send -t), buildRecvCmd (-s -u; `-o encryption=off`
  per trap #4), sizeVariant (-nP read-only sizing twin).
- `sync.go` — planSync (pure compatibility gate: resume token → resume; no
  shared latest GUID → diverged; dest written>0 → dirty; latest==target →
  up-to-date; else incremental with the base resolved BY GUID to the
  source-side snapshot NAME — names can differ across replicas!) +
  customDestPlan (full send to a not-yet-existing dataset) + syncJob (the
  pipeline job: send | mbuffer-if-available | pv-if-available | [ssh] recv;
  remote dests get a per-job ssh ControlMaster authed under the launcher's
  forwarded agent, `touch ssh-ok` releases the launcher (WaitStart), the
  pipeline rides the master; `zfs send -nP` estimate → ./expected, pv bytes
  → ./progress).
- `jobs.go` — job protocol: /tmp/zfstui-job-<id>/ (job.sh; meta.json written
  by job.sh itself via heredoc = single file transfer; status
  `running pid ts|done ts|failed rc ts`; log; progress; expected; ssh-ok).
  One-round-trip launch with job.sh on ssh stdin + setsid; bootstrap
  optionally waits for ssh-ok (WaitStart). waitJob polls status+log tail.
  Job IDs: 6 random base62 chars; time order lives in meta "created".
- `ops.go` — Refresh (errgroup fan-out, limit 8; unreachable hosts recorded,
  cache kept; persists host access) and loadData (cache → Data{States,
  Groups, Hosts, SnapsByReplica, JobsByReplica}); indexJobs sorts by meta
  "created" desc.
- `statusview.go` — plain-text matrix for `status`. CellText shared with the
  TUI: prefix without its trailing slash (`bak@20250409`), markers `+`
  written `!` diverged `~` resumable `%` clone; ahead deliberately has NO
  marker (the newest @name in a row identifies the source).
- `tui.go` (big; all UI) — bubbletea+lipgloss+bubbles(textinput):
  - cursor: curRow -2 = host header (←→ jump host-to-host), -1 = pool
    header, 0.. = group rows; curCol 0 = name column, 1..N = sorted
    "host:pool" columns. Row guide: cursor's row gets bg tint 238 always.
    THREE non-data header rows (tableView emits them; data-row count and
    scrollToCursor subtract 3): host (size only, no %), pool (name + 🧹 if
    scrubbing), and a non-focusable pool fill-bar row (`▰▰▰▱▱` via fillBar,
    dim/border colored, sized to the column).
  - colors: white normal, gray behind/stale, green unsynced snapshots
    (ahead), bright yellow 226 unsnapshotted writes (+), red diverged and
    unhealthy/error pool headers, cyan 14 resumable / section headers /
    status messages, dim clone / unreachable-host columns. Done jobs plain
    white. The user picks colors by feel — expect tweaks.
  - panes: the Datasets/main table has no pane title line; Details keeps a
    segmented title: ──[ id ]──[ info chips ]── (inactive text plain white,
    brackets/rules dim). The active/focused title chip is white-on-dark
    border fill, including its padding spaces; window/dialog titles use the
    same filled title treatment (window titles live in the box border, and are
    host-qualified: `disk h:/dev/sda`, `vdev h:pool/x`, `snapshot h:ds@s`,
    `job h:id`). host header spans append the total pool size ("38T") when wide
    enough (utilisation now lives in the fill-bar row, not the header).
  - **unified object views (view.go) — THE refactor**: ONE builder per object
    type (groupView/datasetView/hostView/poolView/snapshotView/diskView/
    vdevView/jobView, + cellView for multi-replica cells, + textView for plain
    dry-run/failure text) returns `(segs []string, lines []viewLine)`. Each
    builder takes `full bool` and renders TWO ways from the same code:
    * compact (Details pane, full=false): no section headers, no property
      dumps / verbatim zpool-status / SMART; flush tables (cyan column header,
      NO bullets/indent — user wants it dense and flush) with the scan/errors
      and selectable ssh lines drilling UP into the full window.
    * full (interactive window, full=true): cyan centered `── title ──` section
      headers, complete `get all` property maps (propsDump, sources) + verbatim
      text (appendVerbatim) — the old text-modal look, now with a row cursor.
      Table data rows are white; headers cyan. withSectionGaps guarantees a
      blank line above every section header. propsDump drops the SOURCE column
      when the map has no sources (lsblk). stripCtrl scrubs CR/ESC/control
      bytes from job-log & verbatim lines (a bare \r is zero-width to lipgloss
      but resets the terminal cursor → border lands mid-line) — but NOT from
      propsDump (it embeds intentional cyan ANSI).
    Tables come from `vtable(full, title, headers, rows, right, rstyle, open)`;
    `open(i)` returns the row object's `*objView`. Builders are RE-RUN every
    render so spinners / progress / ETA stay live in windows, not just the pane
    (text modals used to be a static snapshot — gone).
  - **window stack** (`Model.windows []*window`): Enter on a selectable row
    pushes a sub-window (`pushView`), Esc/q pops (`popWindow`), focus returns to
    the previous window. updateWindow: ↑↓/jk move the row cursor (or scroll when
    no selectable rows), Enter drills / closes, ←→/hl pan, PgUp/Dn page, g/G
    ends, Esc pops. scrollMove (shared with the pane) is the load-bearing
    nav helper: it free-scrolls past the last parkable row to reveal trailing
    content (props), AND pulls an off-screen cursor onto a visible selectable
    row so PgUp/Dn never strands the cursor on a prior page. windowView renders the top window as a dialogBox overlaid
    via overlayCenter. Enter in the MAIN table pushes directly: dataset cell →
    datasetView, host header → hostView, pool header → poolView; group rows /
    multi-replica cells drill into the Details pane (Tab focus). details.go is
    now just pane glue: currentView (selection→objView), detailsContent (compact
    build), detMove/detSnap/detOpen (push window), detailsBody (8-line render +
    white-bg row cursor). moveSelectable is shared by pane and window.
    Per-object content (chips → segs, body → tables/kv): dataset (mountpoint /
    used / @latest (refer) / +written / encryption; clone/resume notes; JOBS +
    SNAPSHOT tables; full adds kv [mountpoint/space/created/encryption-with-
    `root:` encryptionroot; NO host/type rows — host is in the title] + zfs
    props; `hz` shows explicit "0" not blank in kv space line), group/cell
    (REPLICAS table), host (ssh + DISK table compact; full adds `space:` total/
    free/%used + `uptime:` booted-ts [from HostInfo.BootTime] + vdevs/pools/
    datasets tables + counts, NO verbatims), pool (scan/errors lines + leaf
    VDEV + DATASET compact; full adds health/space kv + zpool status verbatim
    + props), snapshot (kv + props), disk (top: size + smart only; full adds
    lsblk props then `smartctl -a` verbatim LAST), vdev/job (kv + props/script
    + job log). VDEV topology is NOT reconstructed; `zpool status` verbatim is
    the topology view. Tables use measured column widths; avoid fixed
    `%6s`-style columns (break on "1022.5G").
  - `s` snapshot dialog, `y` sync dialog: centered dialogBox (title in the
    border) composited OVER the table via overlayCenter (ANSI-aware
    splicing). Forms: bubbles textinput + checkbox() + button(), Tab/↑↓
    focus cycle, Space toggles, Enter = default action, NO separate confirm
    screen — the form previews the exact command live; in -dry-run buttons
    say "(dry run)" and a textView window shows command/job dir/job.sh instead.
    Sync dialog: members pre-checked iff they need syncing, disabled rows
    show reasons, free-form "new dest" editbox (host:pool/dataset, comma-
    separated → full send, must not exist); editbox focus passes ALL keys to
    textinput. Submit launches one job per dest, polls ~12s, leaves long
    transfers detached.
  - live progress: ensureTicker starts a 150ms tick while runningJobHosts()
    is non-empty; every 20th tick re-polls jobsProbe per host (jobsPollMsg)
    updating Jobs/JobsByReplica in place; running→done/failed transitions
    auto-refresh affected hosts. Cells with running jobs get a braille
    spinner (cellTextLive); details show jobProgressLine "X / Y (Z%)".
  - `h` toggles hidden groups — NOT vim-left (user decision).
- `main.go` — global flags BEFORE the command; first positional arg is the
  command iff it matches one, the rest are host specs. `status` works from
  cache alone; the TUI resolves hosts from sqlite so refresh always works.

## Conventions & user preferences (learned, don't regress)

- No naming-convention config or hardcoded `srv/`/`bak/` anywhere; all
  display logic is derived from data.
- Keep deps minimal; parse zfs output by hand; stdlib `flag`, no cobra.
- Minimal cell markers; no master indicator; usernames never shown in the
  table (access ≠ identity); short job IDs; subtle-but-visible styling.
- Tests: table-driven pure-function tests (derive_test.go `fx` builder,
  sync_test.go `syncFixture` zoo, hostspec_test.go), fixture text for
  parsers, fakeRunner/jobRunner for ssh-free end-to-end, tui_test.go drives
  Update() with tea.KeyMsg directly (no pty needed). New derive/planner
  logic must come with scenario tests.
- Eyeballing layout: write a throwaway `render_eyeball_test.go` printing
  m.View(), run with -v, strip ANSI with sed, delete the file (pattern used
  many times; pty key delivery via `script` is unreliable here, rendering
  one frame works).
- BEWARE silent no-op python str.replace doc/code edits — verify with grep
  after batch edits (bit us twice).
- ALWAYS finish a change by rebuilding the binary the user runs:
  `go build -o zfstui .` (explicit user requirement — they test immediately).
- The user commits; the agent cannot (`.git` is read-only here). Keep
  DESIGN.md updated as you work — explicit user requirement (README.md no
  longer exists; everything lives in DESIGN.md).

## Known warts / next steps

- Old job dirs accumulate in /tmp until reboot/manual rm (safe to delete
  when not running) — auto-pruning planned; keeps details job history noisy.
- Hosts table keeps entries for hosts you stop using (and `status <typo>`
  remembers the typo) until the db is wiped — a `forget` command may come.
- Real-data oddity: `packages` shows as two unrelated groups (no shared
  GUIDs) — correct; re-linking would need a full resend.
- State display string is "synced" (constant StateInSync) — user prefers it
  over "in-sync".
- Pool part of host specs (user@host:pool) is stored/displayed but doesn't
  filter anything yet.
- Disk/vdev discovery follow-ups: `zd*` zvol pseudo-disks are filtered; the
  filter should stay precise (`zd` + digits). Vdev→disk mapping currently
  uses lsblk NAME/KNAME/PATH and by-partlabel aliases; pools using by-id or
  by-path may need explicit alias discovery (`readlink -f /dev/disk/by-*/*`
  or lsblk JSON). SMART output that cannot be mapped to a Disk is dropped by
  design during prototyping; if SMART unexpectedly shows `-`, inspect alias
  mapping before adding fallbacks.
- Next candidates: prune dialog (`d`, DESIGN "Snapshot pruning" — destroy
  snapshots with pairwise-common-base locking), batch `snap`/`sync` CLI,
  jobs pane, `doctor`, job-dir cleanup, XDG paths before release.
