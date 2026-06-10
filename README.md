# zfstui

## Status (2026-06-10)

Design phase, main decisions settled. Architecture, data model, job protocol,
TUI sketch, roadmap and decisions log are in [DESIGN.md](DESIGN.md).
Language: **Go**. Snapshot pruning is in scope (interactive dialog + batch
command); destructive ops (`recv -F`, rollback, dataset destroy) are banned
outright. Next step: M1 — refresh/discovery into sqlite + `status` CLI.

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

## Initial wishes

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
