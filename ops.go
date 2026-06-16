// Package ops holds operations shared by the CLI and the TUI: the per-host
// refresh fan-out and loading derived state from the cache.
package main

import (
	"context"
	"sort"

	"golang.org/x/sync/errgroup"
)

// jobLess orders jobs newest-first by meta "created", with the random ID as a
// stable tiebreak (IDs aren't time-ordered). Shared by indexJobs and the
// host-window jobs table.
func jobLess(a, b JobInfo) bool {
	if a.Meta["created"] != b.Meta["created"] {
		return a.Meta["created"] > b.Meta["created"]
	}
	return a.ID > b.ID
}

// RefreshResult is the outcome of refreshing one host.
type RefreshResult struct {
	Host string
	Err  error
}

// Refresh discovers all hosts in parallel and updates the cache. Unreachable
// hosts are recorded (cached data kept) and reported, not fatal.
func Refresh(ctx context.Context, r Runner, st *Store, hosts []Host) []RefreshResult {
	var eg errgroup.Group
	eg.SetLimit(8)
	results := make([]RefreshResult, len(hosts))
	for i, h := range hosts {
		eg.Go(func() error {
			results[i] = RefreshResult{Host: h.Name, Err: refreshHost(ctx, r, st, h)}
			return nil
		})
	}
	eg.Wait()
	return results
}

func refreshHost(ctx context.Context, r Runner, st *Store, h Host) error {
	// Remember the access spec used, so a bare `zfstui` works next time.
	if err := st.UpsertHostAccess(h); err != nil {
		return err
	}
	out, err := r.Run(ctx, h, DiscoverCommand(h.NeedSudo()))
	if err == nil {
		var hs HostState
		if hs, err = ParseDiscovery(h.Name, string(out)); err == nil {
			return st.PutHostState(hs)
		}
	}
	if dbErr := st.MarkUnreachable(h.Name, err); dbErr != nil {
		return dbErr
	}
	return err
}

// Data is everything the status table / TUI needs, loaded from the cache.
type Data struct {
	States []HostState
	Groups []*Group // hidden overrides already applied
	Hosts  []HostInfo
	// SnapsByReplica indexes snapshots by "host:dataset" (ascending creation).
	SnapsByReplica map[string][]Snapshot
	// JobsByReplica indexes discovered jobs by the "host:dataset" keys they
	// touch (sync: src and dest; snapshot: the dataset), newest job first.
	JobsByReplica map[string][]JobInfo
}

// jobReplicaKeys returns the "host:dataset" keys a job touches.
func jobReplicaKeys(j JobInfo) []string {
	var keys []string
	switch j.Kind {
	case "sync":
		if s := j.Meta["src"]; s != "" {
			keys = append(keys, s)
		}
		if d := j.Meta["dest"]; d != "" {
			keys = append(keys, d)
		}
	case "snapshot":
		if ds := j.Meta["dataset"]; ds != "" {
			keys = append(keys, j.Host+":"+ds)
		}
	}
	return keys
}

func indexJobs(states []HostState) map[string][]JobInfo {
	idx := map[string][]JobInfo{}
	for _, hs := range states {
		for _, j := range hs.Jobs {
			for _, k := range jobReplicaKeys(j) {
				idx[k] = append(idx[k], j)
			}
		}
	}
	for k := range idx {
		jobs := idx[k]
		sort.SliceStable(jobs, func(i, l int) bool { return jobLess(jobs[i], jobs[l]) })
	}
	return idx
}

// loadData reads the cache and derives groups. A non-empty `only` list
// limits the view to those hostnames (cached data for others is ignored).
func loadData(st *Store, only []string) (*Data, error) {
	states, err := st.AllHostStates()
	if err != nil {
		return nil, err
	}
	hosts, err := st.Hosts()
	if err != nil {
		return nil, err
	}
	if len(only) > 0 {
		want := map[string]bool{}
		for _, n := range only {
			want[n] = true
		}
		var fs []HostState
		for _, hs := range states {
			if want[hs.Host] {
				fs = append(fs, hs)
			}
		}
		states = fs
		var fh []HostInfo
		for _, h := range hosts {
			if want[h.Name] {
				fh = append(fh, h)
			}
		}
		hosts = fh
	}
	groups := Groups(states)
	snaps := map[string][]Snapshot{}
	for _, hs := range states {
		for _, s := range hs.Snapshots {
			k := s.Host + ":" + s.Dataset
			snaps[k] = append(snaps[k], s)
		}
	}
	return &Data{States: states, Groups: groups, Hosts: hosts,
		SnapsByReplica: snaps, JobsByReplica: indexJobs(states)}, nil
}
