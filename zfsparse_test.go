package main

import (
	"strings"
	"testing"
)

// prop builds one `get -Hp all` line.
func prop(name, k, v, src string) string {
	return name + "\t" + k + "\t" + v + "\t" + src + "\n"
}

var poolsOut = prop("ssd", "size", "512110190592", "-") +
	prop("ssd", "allocated", "256055095296", "-") +
	prop("ssd", "free", "256055095296", "-") +
	prop("ssd", "capacity", "50", "-") +
	prop("ssd", "fragmentation", "20", "-") +
	prop("ssd", "dedupratio", "1.00", "-") +
	prop("ssd", "health", "ONLINE", "-") +
	prop("ssd", "guid", "1111", "-") +
	prop("ssd", "feature@encryption", "active", "local") +
	prop("tank", "size", "4000787030016", "-") +
	prop("tank", "health", "DEGRADED", "-") +
	prop("tank", "guid", "2222", "-")

func TestParsePools(t *testing.T) {
	pools, err := ParsePools("dev1", poolsOut)
	if err != nil {
		t.Fatal(err)
	}
	if len(pools) != 2 {
		t.Fatalf("got %d pools", len(pools))
	}
	p := pools[0]
	if p.Host != "dev1" || p.Name != "ssd" || p.Size != 512110190592 || p.GUID != "1111" ||
		p.Cap != 50 || p.Frag != 20 || p.Dedup != "1.00" {
		t.Errorf("bad pool: %+v", p)
	}
	if p.Props["feature@encryption"] != "active" || p.PropSrc["feature@encryption"] != "local" {
		t.Errorf("full props not kept: %+v", p.Props)
	}
	if pools[1].Health != "DEGRADED" {
		t.Errorf("bad health: %+v", pools[1])
	}
}

func TestParsePoolsMalformed(t *testing.T) {
	if _, err := ParsePools("dev1", "just one field\n"); err == nil {
		t.Fatal("want error on malformed line")
	}
}

func datasetProps(name string, over map[string]string) string {
	base := map[string]string{
		"type": "filesystem", "guid": "777",
		"used": "1000", "available": "2000", "referenced": "900",
		"creation": "1700000000", "createtxg": "100", "written": "4096",
		"origin": "-", "encryption": "aes-256-gcm", "keystatus": "available",
		"keylocation": "file:///k", "mounted": "yes", "canmount": "on",
		"mountpoint": "/srv/git", "readonly": "off",
		"receive_resume_token": "-", "compression": "zstd", "recordsize": "131072",
	}
	for k, v := range over {
		base[k] = v
	}
	var b strings.Builder
	for k, v := range base {
		b.WriteString(prop(name, k, v, "local"))
	}
	return b.String()
}

var datasetsOut = datasetProps("ssd/srv/git", nil) +
	datasetProps("tank/bak/git", map[string]string{
		"guid": "888", "written": "0", "keystatus": "unavailable", "keylocation": "-",
		"mounted": "no", "canmount": "off", "mountpoint": "-",
		"receive_resume_token": "1-abc-def",
	})

func TestParseDatasets(t *testing.T) {
	dss, err := ParseDatasets("dev1", datasetsOut)
	if err != nil {
		t.Fatal(err)
	}
	if len(dss) != 2 {
		t.Fatalf("got %d datasets", len(dss))
	}
	var d, b Dataset
	for _, x := range dss {
		if x.Name == "ssd/srv/git" {
			d = x
		} else {
			b = x
		}
	}
	if d.Name != "ssd/srv/git" || !d.Encrypted() || !d.Mounted || d.Written != 4096 {
		t.Errorf("bad dataset: %+v", d)
	}
	if d.Pool() != "ssd" || d.NameInPool() != "srv/git" {
		t.Errorf("bad name split: pool=%q nip=%q", d.Pool(), d.NameInPool())
	}
	if d.Origin != "" || d.ResumeToken != "" {
		t.Errorf("'-' should map to empty: %+v", d)
	}
	if d.Props["compression"] != "zstd" || d.PropSrc["compression"] != "local" {
		t.Errorf("props/source not kept: %v %v", d.Props, d.PropSrc)
	}
	if b.Mounted || b.ResumeToken != "1-abc-def" || b.KeyLocation != "" {
		t.Errorf("bad bak dataset: %+v", b)
	}
}

func snapProps(full, guid, used, refer, creation, txg string) string {
	return prop(full, "guid", guid, "-") +
		prop(full, "used", used, "-") +
		prop(full, "referenced", refer, "-") +
		prop(full, "creation", creation, "-") +
		prop(full, "createtxg", txg, "-")
}

var snapsOut = snapProps("ssd/srv/git@20260601", "abc1", "1024", "850", "1764547200", "500") +
	snapProps("ssd/srv/git@20260610", "abc2", "0", "900", "1765324800", "600")

func TestParseSnapshots(t *testing.T) {
	snaps, err := ParseSnapshots("dev1", snapsOut)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 2 {
		t.Fatalf("got %d snapshots", len(snaps))
	}
	var s Snapshot
	for _, x := range snaps {
		if x.Name == "20260601" {
			s = x
		}
	}
	if s.Dataset != "ssd/srv/git" || s.GUID != "abc1" || s.Creation != 1764547200 || s.Refer != 850 {
		t.Errorf("bad snapshot: %+v", s)
	}
}

func TestParseSnapshotsRejectsMissingAt(t *testing.T) {
	if _, err := ParseSnapshots("dev1", prop("ssd/srv/git", "guid", "abc", "-")); err == nil {
		t.Fatal("want error for snapshot without @")
	}
}

func TestParseDiscovery(t *testing.T) {
	out := MarkPools + "\n" + poolsOut + MarkRC + "0===\n" +
		MarkDatasets + "\n" + datasetsOut + MarkRC + "0===\n" +
		MarkSnapshots + "\n" + snapsOut + MarkRC + "0===\n" +
		MarkJobs + "\n" + MarkRC + "0===\n"
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
	if !strings.Contains(cmd, "sudo -n zpool get") || !strings.Contains(cmd, "sudo -n zfs get") {
		t.Errorf("sudo missing: %s", cmd)
	}
	if strings.Contains(DiscoverCommand(false), "sudo") {
		t.Error("unexpected sudo in non-sudo command")
	}
}

const jobsOut = `==J== /tmp/zfstui-job-aa11Qx
running 4242 2026-06-11T10:00:01Z
==M==
{
  "id": "aa11Qx",
  "kind": "sync",
  "src": "ws:tank/srv/git",
  "dest": "cml:root/bak/git"
}
==P==
123456789
==E==
1000000000
==J== /tmp/zfstui-job-bb22Zy
done 2026-06-10T09:05:00Z
==M==
{"id": "bb22Zy", "kind": "snapshot", "dataset": "tank/srv/git", "snapshot": "x"}
==P==
==E==
`

func TestParseJobs(t *testing.T) {
	jobs := ParseJobs("ws", jobsOut)
	if len(jobs) != 2 {
		t.Fatalf("got %d jobs", len(jobs))
	}
	j := jobs[0]
	if j.ID != "aa11Qx" || j.Kind != "sync" || j.State != "running" ||
		j.Progress != 123456789 || j.Expected != 1000000000 ||
		j.Meta["dest"] != "cml:root/bak/git" {
		t.Errorf("job0: %+v", j)
	}
	if jobs[1].State != "done" || jobs[1].Kind != "snapshot" || jobs[1].Progress != 0 {
		t.Errorf("job1: %+v", jobs[1])
	}
}

func TestParseJobsEmptyAndGarbage(t *testing.T) {
	if jobs := ParseJobs("ws", ""); len(jobs) != 0 {
		t.Errorf("empty: %v", jobs)
	}
	// A half-written dir (no meta yet) still yields an entry.
	jobs := ParseJobs("ws", "==J== /tmp/zfstui-job-x\n==M==\n==P==\n==E==\n")
	if len(jobs) != 1 || jobs[0].State != "unknown" {
		t.Errorf("partial: %+v", jobs)
	}
}
