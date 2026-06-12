package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// fakeRunner serves canned discovery output per host, simulating the README's
// example fleet without ssh or zfs.
type fakeRunner struct {
	out map[string]string // host -> discovery stdout
}

func (f *fakeRunner) Run(_ context.Context, h Host, cmd string) ([]byte, error) {
	out, ok := f.out[h.Name]
	if !ok {
		return nil, fmt.Errorf("connect to %s: timeout", h.Name)
	}
	return []byte(out), nil
}

func discovery(pools, datasets, snapshots string) string {
	sec := func(mark, body string) string {
		return mark + "\n" + body + MarkRC + "0===\n"
	}
	return sec(MarkPools, pools) +
		sec(MarkDatasets, datasets) +
		sec(MarkSnapshots, snapshots) +
		sec(MarkJobs, "")
}

func dsLine(name string, written int64) string {
	var b strings.Builder
	for _, kv := range [][2]string{
		{"type", "filesystem"}, {"guid", name + "-guid"},
		{"used", "10"}, {"available", "20"}, {"referenced", "5"},
		{"creation", "1700000000"}, {"createtxg", "1"},
		{"written", fmt.Sprint(written)},
		{"encryption", "aes-256-gcm"}, {"keystatus", "available"},
		{"canmount", "off"}, {"compression", "zstd"}, {"recordsize", "131072"},
	} {
		fmt.Fprintf(&b, "%s\t%s\t%s\t-\n", name, kv[0], kv[1])
	}
	return b.String()
}

func snapLine(ds, name, guid string, creation int64) string {
	full := ds + "@" + name
	var b strings.Builder
	for _, kv := range [][2]string{
		{"guid", guid}, {"used", "0"}, {"referenced", "7"},
		{"creation", fmt.Sprint(creation)}, {"createtxg", "1"},
	} {
		fmt.Fprintf(&b, "%s\t%s\t%s\t-\n", full, kv[0], kv[1])
	}
	return b.String()
}

func TestRefreshAndStatusEndToEnd(t *testing.T) {
	poolProps := func(name, guid string) string {
		var b strings.Builder
		for _, kv := range [][2]string{
			{"size", "100"}, {"allocated", "40"}, {"free", "60"},
			{"health", "ONLINE"}, {"guid", guid},
		} {
			fmt.Fprintf(&b, "%s\t%s\t%s\t-\n", name, kv[0], kv[1])
		}
		return b.String()
	}
	pools1 := poolProps("ssd", "1")
	pools2 := poolProps("ssd", "2") + poolProps("tank", "3")

	runner := &fakeRunner{out: map[string]string{
		"dev1": discovery(pools1,
			dsLine("ssd/srv/git", 4096),
			snapLine("ssd/srv/git", "20260501", "g1", 100)+
				snapLine("ssd/srv/git", "20260601", "g2", 200)),
		"dev2": discovery(pools2,
			dsLine("ssd/bak/git", 0)+dsLine("tank/bak/git", 0),
			snapLine("ssd/bak/git", "20260501", "g1", 100)+
				snapLine("ssd/bak/git", "20260601", "g2", 200)+
				snapLine("tank/bak/git", "20260501", "g1", 100)),
		// "hetzner" missing => unreachable
	}}

	st, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	hosts := []Host{{Name: "dev1"}, {Name: "dev2"}, {Name: "hetzner"}}
	var failed []string
	for _, res := range Refresh(context.Background(), runner, st, hosts) {
		if res.Err != nil {
			failed = append(failed, res.Host)
		}
	}
	if len(failed) != 1 || failed[0] != "hetzner" {
		t.Fatalf("failed hosts: %v", failed)
	}

	data, err := loadData(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := Status(data.Groups)
	t.Log("\n" + out)
	if !strings.Contains(out, "git") ||
		!strings.Contains(out, "srv@20260601+") || // unsnapshotted writes
		!strings.Contains(out, "bak@20260501") { // the stale tank backup
		t.Errorf("unexpected status output:\n%s", out)
	}

	infos, err := st.Hosts()
	if err != nil {
		t.Fatal(err)
	}
	var unreachable []string
	for _, h := range infos {
		if !h.Reachable {
			unreachable = append(unreachable, h.Name)
		}
	}
	if len(unreachable) != 1 || unreachable[0] != "hetzner" {
		t.Errorf("unreachable: %v", unreachable)
	}
}

func (f *fakeRunner) RunInput(ctx context.Context, h Host, cmd string, stdin []byte, sshExtra ...string) ([]byte, error) {
	return f.Run(ctx, h, cmd)
}
