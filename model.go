// Package model holds the inventory data structures shared by all layers:
// what exists on the hosts (pools, datasets, snapshots) and what we derive
// from it (replication groups, per-replica states).
package main

// Host is one entry from the inventory config.
type Host struct {
	Name string // inventory key, also the default ssh destination
	Addr string // optional override for the ssh destination
	Port int    // optional
	User string // optional ssh user
	Sudo bool   // wrap zfs/zpool in sudo
	// Separate sudo-capable users for send/recv sides; empty means User.
	SendUser string
	RecvUser string
	SSHOpts  []string // extra raw ssh options
}

// Dest returns the ssh destination ([user@]addr) for the host.
func (h Host) Dest() string {
	addr := h.Addr
	if addr == "" {
		addr = h.Name
	}
	if h.User != "" {
		return h.User + "@" + addr
	}
	return addr
}

// Pool is one zpool on a host.
type Pool struct {
	Host   string
	Name   string
	GUID   string
	Size   int64
	Alloc  int64
	Free   int64
	Health string
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
	Creation  int64 // unix seconds
	CreateTXG int64
}

// HostState is everything discovered on one host in one refresh.
type HostState struct {
	Host      string
	Pools     []Pool
	Datasets  []Dataset
	Snapshots []Snapshot
}

// ReplicaState classifies one replica within its group.
type ReplicaState string

const (
	StateInSync    ReplicaState = "in-sync"
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
