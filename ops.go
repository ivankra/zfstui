// Package ops holds operations shared by the CLI and the TUI: the per-host
// refresh fan-out and loading derived state from the cache.
package main

import (
	"context"

	"golang.org/x/sync/errgroup"
)

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
	out, err := r.Run(ctx, h, DiscoverCommand(h.Sudo))
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
}

// Load reads the cache and derives groups.
func loadData(st *Store) (*Data, error) {
	states, err := st.AllHostStates()
	if err != nil {
		return nil, err
	}
	groups := Groups(states)
	hosts, err := st.Hosts()
	if err != nil {
		return nil, err
	}
	snaps := map[string][]Snapshot{}
	for _, hs := range states {
		for _, s := range hs.Snapshots {
			k := s.Host + ":" + s.Dataset
			snaps[k] = append(snaps[k], s)
		}
	}
	return &Data{States: states, Groups: groups, Hosts: hosts, SnapsByReplica: snaps}, nil
}
