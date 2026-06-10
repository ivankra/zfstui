package main

import (
	"errors"
	"path/filepath"
	"testing"
)

func sample(host string) HostState {
	return HostState{
		Host: host,
		Pools: []Pool{
			{Host: host, Name: "ssd", GUID: "p1", Size: 100, Alloc: 40, Free: 60, Health: "ONLINE"},
		},
		Datasets: []Dataset{
			{Host: host, Name: "ssd/srv/git", Type: "filesystem", GUID: "d1",
				Written: 4096, Encryption: "aes-256-gcm", Mounted: true, Mountpoint: "/srv/git"},
		},
		Snapshots: []Snapshot{
			{Host: host, Dataset: "ssd/srv/git", Name: "20260601", GUID: "s1", Creation: 100},
			{Host: host, Dataset: "ssd/srv/git", Name: "20260610", GUID: "s2", Creation: 200},
		},
	}
}

func TestRoundTrip(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.PutHostState(sample("dev1")); err != nil {
		t.Fatal(err)
	}
	// Second put replaces, not duplicates.
	if err := s.PutHostState(sample("dev1")); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkUnreachable("dev2", errors.New("timeout")); err != nil {
		t.Fatal(err)
	}

	states, err := s.AllHostStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 {
		t.Fatalf("got %d host states", len(states))
	}
	st := states[0]
	if len(st.Pools) != 1 || len(st.Datasets) != 1 || len(st.Snapshots) != 2 {
		t.Fatalf("bad counts: %+v", st)
	}
	d := st.Datasets[0]
	if !d.Mounted || d.Written != 4096 || d.Encryption != "aes-256-gcm" {
		t.Errorf("dataset round trip: %+v", d)
	}

	hosts, err := s.Hosts()
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 2 {
		t.Fatalf("got %d hosts", len(hosts))
	}
	if !hosts[0].Reachable || hosts[0].Name != "dev1" {
		t.Errorf("dev1: %+v", hosts[0])
	}
	if hosts[1].Reachable || hosts[1].LastError != "timeout" {
		t.Errorf("dev2: %+v", hosts[1])
	}
}
