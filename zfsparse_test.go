package main

import (
	"encoding/base64"
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
		MarkDisks + "\n" + disksOut + MarkRC + "0===\n" +
		MarkDatasets + "\n" + datasetsOut + MarkRC + "0===\n" +
		MarkSnapshots + "\n" + snapsOut + MarkRC + "0===\n" +
		MarkJobs + "\n" + MarkRC + "0===\n" +
		MarkHost + "\nbtime 1700000000\nmachine-id 4c0ffee\n" + MarkRC + "0===\n"
	st, err := ParseDiscovery("dev1", out)
	if err != nil {
		t.Fatal(err)
	}
	if st.BootTime != 1700000000 || st.MachineID != "4c0ffee" {
		t.Errorf("host scalars: boot %d machine-id %q", st.BootTime, st.MachineID)
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

// jobLine builds one jobsProbe output line: dir + base64 key=value tokens.
func jobLine(dir, status, meta, progress, expected, log string) string {
	e := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	return dir + " status=" + e(status) + " meta=" + e(meta) +
		" progress=" + e(progress) + " expected=" + e(expected) + " log=" + e(log)
}

func TestParseJobs(t *testing.T) {
	// The log deliberately contains a tab and a fake "==J==" marker line to
	// prove base64 framing can't be aliased by arbitrary content.
	out := jobLine("/tmp/zfstui-job-aa11Qx",
		"running 4242 2026-06-11T10:00:01Z",
		`{"id":"aa11Qx","kind":"sync","src":"ws:tank/srv/git","dest":"cml:root/bak/git"}`,
		"123456789", "1000000000",
		"sending tank/srv/git@20260611\n==J== not a real marker\tnor\tthese tabs") + "\n" +
		jobLine("/tmp/zfstui-job-bb22Zy",
			"done 2026-06-10T09:05:00Z",
			`{"id":"bb22Zy","kind":"snapshot","dataset":"tank/srv/git","snapshot":"x"}`,
			"", "", "") + "\n"

	jobs := ParseJobs("ws", out)
	if len(jobs) != 2 {
		t.Fatalf("got %d jobs", len(jobs))
	}
	j := jobs[0]
	if j.ID != "aa11Qx" || j.Kind != "sync" || j.State != "running" ||
		j.Progress != 123456789 || j.Expected != 1000000000 ||
		j.Meta["dest"] != "cml:root/bak/git" {
		t.Errorf("job0: %+v", j)
	}
	if j.Log != "sending tank/srv/git@20260611\n==J== not a real marker\tnor\tthese tabs" {
		t.Errorf("job0 log: %q", j.Log)
	}
	if jobs[1].State != "done" || jobs[1].Kind != "snapshot" || jobs[1].Progress != 0 || jobs[1].Log != "" {
		t.Errorf("job1: %+v", jobs[1])
	}
}

func TestParseJobsEmptyAndGarbage(t *testing.T) {
	if jobs := ParseJobs("ws", ""); len(jobs) != 0 {
		t.Errorf("empty: %v", jobs)
	}
	// Shell noise / blank lines without the dir prefix are ignored.
	if jobs := ParseJobs("ws", "bash: base64: not found\n\n"); len(jobs) != 0 {
		t.Errorf("garbage: %v", jobs)
	}
	// A half-written dir (empty fields) still yields an entry, state unknown.
	jobs := ParseJobs("ws", jobLine("/tmp/zfstui-job-x", "", "", "", "", "")+"\n")
	if len(jobs) != 1 || jobs[0].State != "unknown" {
		t.Errorf("partial: %+v", jobs)
	}
}

// Modeled on the user's real `zpool status` + vdev-props output (raidz).
const disksOut = `==P== ssd
root-0	state	ONLINE	-
root-0	parent	-	-
raidz1-0	state	ONLINE	-
raidz1-0	parent	root-0	-
raidz1-0	children	/dev/disk/by-partlabel/HG10-2JNX	-
HG10-2JNX	state	ONLINE	-
HG10-2JNX	parent	raidz-0	-
HG10-2JNX	path	/dev/disk/by-partlabel/HG10-2JNX	-
HG10-2JNX	read_errors	0	-
HG10-2JNX	write_errors	0	-
HG10-2JNX	checksum_errors	3	-
HG10-2JNX	size	0	-
HG10-2JNX	psize	10000829251584	-
==S==
  pool: ssd
 state: ONLINE
  scan: scrub repaired 0B in 11:11:17 with 0 errors on Sun May 10 11:35:20 2026
config:

	NAME           STATE     READ WRITE CKSUM
	ssd            ONLINE       0     0     0

errors: No known data errors
==LSBLK==
NAME="sda" KNAME="sda" TYPE="disk" PKNAME="" PATH="/dev/sda" SIZE="10000831348736" MODEL="HGST HUH721010ALE600" SERIAL="E4GG2JNX" WWN="0x5000cca268c666b8" TRAN="sata" ROTA="1" PARTLABEL="" PARTUUID=""
NAME="sda1" KNAME="sda1" TYPE="part" PKNAME="sda" PATH="/dev/sda1" SIZE="10000829251584" MODEL="" SERIAL="" WWN="" TRAN="" ROTA="1" PARTLABEL="HG10-2JNX" PARTUUID="part-a"
NAME="nvme0n1" KNAME="nvme0n1" TYPE="disk" PKNAME="" PATH="/dev/nvme0n1" SIZE="2000398934016" MODEL="Samsung SSD 970 EVO Plus 2TB" SERIAL="S4J4NJ0N805480Y" WWN="eui.0025385801400001" TRAN="nvme" ROTA="0" PARTLABEL="" PARTUUID=""
NAME="zd0" KNAME="zd0" TYPE="disk" PKNAME="" PATH="/dev/zd0" SIZE="10737418240" MODEL="" SERIAL="" WWN="" TRAN="" ROTA="0" PARTLABEL="" PARTUUID=""
==SMART==
==D== /dev/sda
smartctl 7.4 ...
SMART overall-health self-assessment test result: PASSED
==D== /dev/nvme0n1
smartctl 7.4 ...
Critical Warning: 0x00
`

func TestParseDisks(t *testing.T) {
	vdevs, statusTexts, disks := ParseDisks("sta", disksOut)
	if len(vdevs) != 3 {
		t.Fatalf("got %d vdevs: %+v", len(vdevs), vdevs)
	}
	disk := vdevs[2]
	if disk.Name != "HG10-2JNX" || disk.Parent != "raidz-0" || disk.Pool != "ssd" ||
		disk.CErr != 3 || disk.Size != 10000829251584 || disk.State != "ONLINE" ||
		disk.DiskID != "wwn:0x5000cca268c666b8" {
		t.Errorf("disk vdev: %+v", disk)
	}
	if vdevs[0].Parent != "" { // root-0's "-" parent normalizes to empty
		t.Errorf("root parent: %+v", vdevs[0])
	}
	if !strings.Contains(statusTexts["ssd"], "scrub repaired 0B in 11:11:17") ||
		!strings.Contains(statusTexts["ssd"], "No known data errors") {
		t.Errorf("status text: %q", statusTexts["ssd"])
	}
	if len(disks) != 2 || disks[0].Dev != "/dev/sda" ||
		disks[0].ID != "wwn:0x5000cca268c666b8" ||
		!strings.Contains(disks[0].Smart, "PASSED") ||
		!strings.Contains(disks[1].Smart, "Critical Warning") {
		t.Errorf("disks: %+v", disks)
	}
	// sda1 hangs off sda; the partition row is attached and persisted.
	if len(disks[0].Parts) != 1 || disks[0].Parts[0]["NAME"] != "sda1" ||
		disks[0].Props["_parts"] == "" {
		t.Errorf("disk partitions: %+v", disks[0].Parts)
	}
	if len(disks[1].Parts) != 0 {
		t.Errorf("nvme0n1 should have no partitions: %+v", disks[1].Parts)
	}
	if isPseudoDiskName("zdata") || !isPseudoDiskName("zd0") {
		t.Error("pseudo disk filter should only match zd followed by digits")
	}
}

// Old zfs without vdev properties: the prop table is empty but the verbatim
// zpool status still surfaces (explicit user requirement).
func TestParseDisksNoVdevProps(t *testing.T) {
	out := "==P== tank\n==S==\n  pool: tank\n state: ONLINE\nerrors: No known data errors\n==SMART==\n"
	vdevs, statusTexts, disks := ParseDisks("h", out)
	if len(vdevs) != 0 || len(disks) != 0 {
		t.Errorf("vdevs=%v disks=%v", vdevs, disks)
	}
	if !strings.Contains(statusTexts["tank"], "state: ONLINE") {
		t.Errorf("status: %q", statusTexts["tank"])
	}
}
