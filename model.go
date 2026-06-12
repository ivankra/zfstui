// Inventory data structures shared by all layers: what exists on the hosts
// (pools, datasets, snapshots) and what we derive from it (replication
// groups, per-replica states).
package main

import "strconv"

// Host is one known host. Name (the hostname) is the sole identity — db key,
// column label, dedup key. Everything else is access specification, parsed
// from ssh/scp-style URLs (see parseHostSpec) and remembered in sqlite.
// Aliasing/jump hosts are ~/.ssh/config's job, not ours.
type Host struct {
	Name string // hostname: THE identity
	User string // ssh user; empty means root
	Port int    // optional
	Pool string // optional pool restriction from the URL path (informational for now)
	// Separate sudo-capable users for send/recv sides; empty means User.
	SendUser string
	RecvUser string
}

// NeedSudo: ssh-ing as root needs no sudo; any other user does.
func (h Host) NeedSudo() bool { return h.EffUser() != "root" }

// EffUser is the ssh user with the root default applied.
func (h Host) EffUser() string {
	if h.User == "" {
		return "root"
	}
	return h.User
}

// Dest returns the ssh destination (user@host).
func (h Host) Dest() string {
	return h.EffUser() + "@" + h.Name
}

// URL reassembles the access spec for display: the short scp form
// (user@host[:pool]) when possible, the ssh:// form when port or extra
// options are involved.
func (h Host) URL() string {
	var opts []string
	if h.SendUser != "" {
		opts = append(opts, "send_user="+h.SendUser)
	}
	if h.RecvUser != "" {
		opts = append(opts, "recv_user="+h.RecvUser)
	}
	if h.Port == 0 && len(opts) == 0 {
		s := h.EffUser() + "@" + h.Name
		if h.Pool != "" {
			s += ":" + h.Pool
		}
		return s
	}
	s := "ssh://" + h.EffUser()
	for _, o := range opts {
		s += ";" + o
	}
	s += "@" + h.Name
	if h.Port != 0 {
		s += ":" + strconv.Itoa(h.Port)
	}
	if h.Pool != "" {
		s += "/" + h.Pool
	}
	return s
}

// SameAccess reports whether two specs for the same host agree.
func (h Host) SameAccess(o Host) bool {
	return h.EffUser() == o.EffUser() && h.Port == o.Port && h.Pool == o.Pool &&
		h.SendUser == o.SendUser && h.RecvUser == o.RecvUser
}

// Pool is one zpool on a host. Typed fields are parsed out of Props
// (`zpool get -Hp all`); Props/PropSrc carry everything for detail views.
type Pool struct {
	Host    string
	Name    string
	GUID    string
	Size    int64
	Alloc   int64
	Free    int64
	Cap     int64  // capacity %
	Frag    int64  // fragmentation %
	Dedup   string // dedupratio
	Health  string
	Props   map[string]string
	PropSrc map[string]string
}

// Dataset is a filesystem or volume on a host. Snapshots live separately.
type Dataset struct {
	Host        string
	Name        string // full zfs name including pool, e.g. "ssd/srv/git"
	Type        string // filesystem | volume
	GUID        string // dataset guid (differs per replica, unlike snapshot guids)
	Used        int64
	Avail       int64
	Refer       int64
	Creation    int64 // unix seconds
	CreateTXG   int64
	Written     int64  // bytes written since latest snapshot
	Origin      string // non-empty => clone of that dataset@snap
	Encryption  string // "off" or an algorithm
	KeyStatus   string
	KeyLocation string
	Mounted     bool
	CanMount    string
	Mountpoint  string
	ReadOnly    bool
	ResumeToken string // receive_resume_token; non-empty => interrupted recv
	Compression string
	RecordSize  int64
	Props       map[string]string // full `zfs get all` map (display)
	PropSrc     map[string]string // property sources (local/inherited/...)
}

// Pool returns the pool component of the dataset name.
func (d Dataset) Pool() string {
	for i := 0; i < len(d.Name); i++ {
		if d.Name[i] == '/' {
			return d.Name[:i]
		}
	}
	return d.Name
}

// NameInPool returns the dataset name with the pool prefix stripped
// ("" for the pool's root dataset).
func (d Dataset) NameInPool() string {
	for i := 0; i < len(d.Name); i++ {
		if d.Name[i] == '/' {
			return d.Name[i+1:]
		}
	}
	return ""
}

// Encrypted reports whether sends from this dataset must be raw (-w).
func (d Dataset) Encrypted() bool {
	return d.Encryption != "" && d.Encryption != "off"
}

// Snapshot of a dataset on a host.
type Snapshot struct {
	Host      string
	Dataset   string // full dataset name the snapshot belongs to
	Name      string // snapshot part only (after "@")
	GUID      string // preserved across send/recv: the cross-host identity
	Used      int64
	Refer     int64
	Creation  int64 // unix seconds
	CreateTXG int64
	Props     map[string]string
	PropSrc   map[string]string
}

// VDev is one vdev (root, raidz/mirror group, disk) from `zpool get all
// <pool> all-vdevs` (OpenZFS ≥ 2.2 vdev properties).
type VDev struct {
	Host    string
	Pool    string
	Name    string // vdev name: "root-0", "raidz1-0", "nvme0n1p1", …
	Parent  string // parent vdev name ("" for the root)
	DiskID  string // stable physical disk ID for leaf vdevs, when known
	State   string
	Path    string // /dev path for leaf disks
	Size    int64
	Alloc   int64
	Free    int64
	Frag    int64
	RErr    int64 // read_errors
	WErr    int64 // write_errors
	CErr    int64 // checksum_errors
	Props   map[string]string
	PropSrc map[string]string
}

// Disk is one physical block device on a host. Partition/path vdevs point
// back to DiskID when discovery can map them through lsblk.
type Disk struct {
	Host       string
	ID         string // stable: prefer WWN, then serial, then last-resort dev
	Dev        string // /dev/sda, /dev/nvme0n1, …
	KName      string // kernel name, e.g. sda
	Model      string
	Serial     string
	WWN        string
	Size       int64
	Transport  string
	Rotational bool
	Smart      string            // verbatim smartctl -a output
	Props      map[string]string // full lsblk -P row
}

// HostState is everything discovered on one host in one refresh.
type HostState struct {
	Host      string
	Pools     []Pool
	Datasets  []Dataset
	Snapshots []Snapshot
	VDevs     []VDev
	Disks     []Disk
	Jobs      []JobInfo
}

// JobInfo is a zfstui job discovered in /tmp/zfstui-job-* on a host.
type JobInfo struct {
	Host     string
	ID       string
	Dir      string
	Kind     string            // snapshot | sync
	State    string            // running | done | failed | unknown
	Status   string            // raw status line (pid, rc, timestamps)
	Meta     map[string]string // from meta.json (src, dest, dataset, …)
	Progress int64             // bytes transferred so far (pv), 0 if unknown
	Expected int64             // estimated total bytes (zfs send -nP), 0 if unknown
}

// ReplicaState classifies one replica within its group.
type ReplicaState string

const (
	StateInSync    ReplicaState = "synced"
	StateAhead     ReplicaState = "ahead"
	StateBehind    ReplicaState = "behind"
	StateDiverged  ReplicaState = "diverged"
	StateClone     ReplicaState = "clone"
	StateResumable ReplicaState = "resumable" // partial recv; orthogonal but exclusive in v1 display
)

// Replica is a group member: a dataset plus its derived standing.
type Replica struct {
	Dataset *Dataset
	State   ReplicaState
	// CellPrefix is the replica's name remainder once pool and the group's
	// common suffix are stripped (e.g. "srv/", "bak/"); display-only.
	CellPrefix string
	// Latest snapshot on this replica (nil if none).
	Latest *Snapshot
	// Written > 0 means unsnapshotted local writes (copied from Dataset).
	Written int64
}

// Group is a set of datasets connected by shared snapshot GUIDs.
type Group struct {
	DisplayName string
	Members     []*Replica
	// LatestName/LatestCreation describe the group-wide newest snapshot.
	LatestName     string
	LatestCreation int64
	Diverged       bool // any diverged pair within the group
	// Hidden marks groups not shown by default: no member has any snapshot
	// (system/plumbing datasets), unless overridden by the user (TUI `h`).
	Hidden bool
}
