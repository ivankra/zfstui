package zfsout

import (
	"strings"
	"testing"
)

const poolsOut = "ssd\t512110190592\t256055095296\t256055095296\tONLINE\t1111\n" +
	"tank\t4000787030016\t2000393515008\t2000393515008\tDEGRADED\t2222\n"

func TestParsePools(t *testing.T) {
	pools, err := ParsePools("dev1", poolsOut)
	if err != nil {
		t.Fatal(err)
	}
	if len(pools) != 2 {
		t.Fatalf("got %d pools", len(pools))
	}
	p := pools[0]
	if p.Host != "dev1" || p.Name != "ssd" || p.Size != 512110190592 || p.GUID != "1111" {
		t.Errorf("bad pool: %+v", p)
	}
	if pools[1].Health != "DEGRADED" {
		t.Errorf("bad health: %+v", pools[1])
	}
}

func TestParsePoolsBadColumns(t *testing.T) {
	if _, err := ParsePools("dev1", "ssd\t123\n"); err == nil {
		t.Fatal("want error on wrong column count")
	}
}

// One encrypted mounted dataset, one bak (unmounted, resume token), in -H -p form.
const datasetsOut = "ssd/srv/git\tfilesystem\t777\t1000\t2000\t900\t1700000000\t100\t4096\t-\t" +
	"aes-256-gcm\tavailable\tfile:///k\tyes\ton\t/srv/git\toff\t-\tzstd\t131072\n" +
	"tank/bak/git\tfilesystem\t888\t1000\t2000\t900\t1700000001\t101\t0\t-\t" +
	"aes-256-gcm\tunavailable\t-\tno\toff\t-\toff\t1-abc-def\tzstd\t131072\n"

func TestParseDatasets(t *testing.T) {
	dss, err := ParseDatasets("dev1", datasetsOut)
	if err != nil {
		t.Fatal(err)
	}
	if len(dss) != 2 {
		t.Fatalf("got %d datasets", len(dss))
	}
	d := dss[0]
	if d.Name != "ssd/srv/git" || !d.Encrypted() || !d.Mounted || d.Written != 4096 {
		t.Errorf("bad dataset: %+v", d)
	}
	if d.Pool() != "ssd" || d.NameInPool() != "srv/git" {
		t.Errorf("bad name split: pool=%q nip=%q", d.Pool(), d.NameInPool())
	}
	if d.Origin != "" || d.ResumeToken != "" {
		t.Errorf("'-' should map to empty: %+v", d)
	}
	b := dss[1]
	if b.Mounted || b.ResumeToken != "1-abc-def" || b.KeyLocation != "" {
		t.Errorf("bad bak dataset: %+v", b)
	}
}

const snapsOut = "ssd/srv/git@20260601\tabc1\t1024\t1764547200\t500\n" +
	"ssd/srv/git@20260610\tabc2\t0\t1765324800\t600\n"

func TestParseSnapshots(t *testing.T) {
	snaps, err := ParseSnapshots("dev1", snapsOut)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 2 {
		t.Fatalf("got %d snapshots", len(snaps))
	}
	s := snaps[0]
	if s.Dataset != "ssd/srv/git" || s.Name != "20260601" || s.GUID != "abc1" || s.Creation != 1764547200 {
		t.Errorf("bad snapshot: %+v", s)
	}
}

func TestParseSnapshotsRejectsMissingAt(t *testing.T) {
	if _, err := ParseSnapshots("dev1", "ssd/srv/git\tabc\t0\t1\t2\n"); err == nil {
		t.Fatal("want error for snapshot without @")
	}
}

func TestParseDiscovery(t *testing.T) {
	out := MarkPools + "\n" + poolsOut + MarkRC + "0===\n" +
		MarkDatasets + "\n" + datasetsOut + MarkRC + "0===\n" +
		MarkSnapshots + "\n" + snapsOut + MarkRC + "0===\n"
	st, err := ParseDiscovery("dev1", out)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Pools) != 2 || len(st.Datasets) != 2 || len(st.Snapshots) != 2 {
		t.Fatalf("bad counts: %d pools %d datasets %d snapshots",
			len(st.Pools), len(st.Datasets), len(st.Snapshots))
	}
}

func TestParseDiscoveryNonzeroRC(t *testing.T) {
	out := MarkPools + "\n" + MarkRC + "127===\n"
	if _, err := ParseDiscovery("dev1", out); err == nil ||
		!strings.Contains(err.Error(), "127") {
		t.Fatalf("want rc error, got %v", err)
	}
}

func TestParseDiscoveryMissingSection(t *testing.T) {
	out := MarkPools + "\n" + poolsOut + MarkRC + "0===\n"
	if _, err := ParseDiscovery("dev1", out); err == nil {
		t.Fatal("want error for missing sections")
	}
}

func TestDiscoverCommandSudo(t *testing.T) {
	cmd := DiscoverCommand(true)
	if !strings.Contains(cmd, "sudo -n zpool list") || !strings.Contains(cmd, "sudo -n zfs list") {
		t.Errorf("sudo missing: %s", cmd)
	}
	if strings.Contains(DiscoverCommand(false), "sudo") {
		t.Error("unexpected sudo in non-sudo command")
	}
}
