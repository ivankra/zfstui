package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"ivank.me/zfstui/internal/derive"
	"ivank.me/zfstui/internal/model"
	"ivank.me/zfstui/internal/store"
	"ivank.me/zfstui/internal/view"
	"ivank.me/zfstui/internal/zfsout"
)

// fakeRunner serves canned discovery output per host, simulating the README's
// example fleet without ssh or zfs.
type fakeRunner struct {
	out map[string]string // host -> discovery stdout
}

func (f *fakeRunner) Run(_ context.Context, h model.Host, cmd string) ([]byte, error) {
	out, ok := f.out[h.Name]
	if !ok {
		return nil, fmt.Errorf("connect to %s: timeout", h.Name)
	}
	return []byte(out), nil
}

func discovery(pools, datasets, snapshots string) string {
	sec := func(mark, body string) string {
		return mark + "\n" + body + zfsout.MarkRC + "0===\n"
	}
	return sec(zfsout.MarkPools, pools) +
		sec(zfsout.MarkDatasets, datasets) +
		sec(zfsout.MarkSnapshots, snapshots)
}

func dsLine(name string, written int64) string {
	return fmt.Sprintf("%s\tfilesystem\t%s-guid\t10\t20\t5\t1700000000\t1\t%d\t-\t"+
		"aes-256-gcm\tavailable\t-\tno\toff\t-\toff\t-\tzstd\t131072\n",
		name, name, written)
}

func snapLine(ds, name, guid string, creation int64) string {
	return fmt.Sprintf("%s@%s\t%s\t0\t%d\t1\n", ds, name, guid, creation)
}

func TestRefreshAndStatusEndToEnd(t *testing.T) {
	pools1 := "ssd\t100\t40\t60\tONLINE\t1\n"
	pools2 := "ssd\t100\t40\t60\tONLINE\t2\ntank\t900\t100\t800\tONLINE\t3\n"

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

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	hosts := []model.Host{{Name: "dev1"}, {Name: "dev2"}, {Name: "hetzner"}}
	var failed []string
	for _, h := range hosts {
		if err := refreshHost(context.Background(), runner, st, h); err != nil {
			failed = append(failed, h.Name)
		}
	}
	if len(failed) != 1 || failed[0] != "hetzner" {
		t.Fatalf("failed hosts: %v", failed)
	}

	states, err := st.AllHostStates()
	if err != nil {
		t.Fatal(err)
	}
	out := view.Status(derive.Groups(states))
	t.Log("\n" + out)
	if !strings.Contains(out, "git") ||
		!strings.Contains(out, "srv/@20260601+") || // unsnapshotted writes
		!strings.Contains(out, "bak/@20260501") { // the stale tank backup
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
