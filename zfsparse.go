// Package zfsout parses the machine-readable (-H -p) output of zpool/zfs
// commands. Hand-rolled on purpose: tab-separated, "-" means absent.
package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Column lists for the remote commands. Parsers below must stay in lockstep.
const (
	PoolCols     = "name,size,alloc,free,health,guid"
	DatasetCols  = "name,type,guid,used,avail,refer,creation,createtxg,written,origin,encryption,keystatus,keylocation,mounted,canmount,mountpoint,readonly,receive_resume_token,compression,recordsize"
	SnapshotCols = "name,guid,used,creation,createtxg"
)

// fields splits one -H output line and verifies the column count.
func fields(line string, want int) ([]string, error) {
	f := strings.Split(line, "\t")
	if len(f) != want {
		return nil, fmt.Errorf("expected %d tab-separated fields, got %d in %q", want, len(f), line)
	}
	return f, nil
}

// num parses a -p numeric field; "-" and "" mean 0.
func num(s string) (int64, error) {
	if s == "-" || s == "" {
		return 0, nil
	}
	return strconv.ParseInt(s, 10, 64)
}

// str normalizes a string field; "-" means absent.
func str(s string) string {
	if s == "-" {
		return ""
	}
	return s
}

func boolish(s string) bool {
	return s == "yes" || s == "on"
}

// nonEmptyLines splits raw command output into trimmed non-empty lines.
func nonEmptyLines(out string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimRight(l, "\r")
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// ParsePools parses `zpool list -H -p -o <PoolCols>` output.
func ParsePools(host, out string) ([]Pool, error) {
	var pools []Pool
	for _, line := range nonEmptyLines(out) {
		f, err := fields(line, 6)
		if err != nil {
			return nil, err
		}
		size, err1 := num(f[1])
		alloc, err2 := num(f[2])
		free, err3 := num(f[3])
		for _, err := range []error{err1, err2, err3} {
			if err != nil {
				return nil, fmt.Errorf("pool %q: %w", f[0], err)
			}
		}
		pools = append(pools, Pool{
			Host: host, Name: f[0], Size: size, Alloc: alloc, Free: free,
			Health: f[4], GUID: str(f[5]),
		})
	}
	return pools, nil
}

// ParseDatasets parses `zfs list -H -p -t filesystem,volume -o <DatasetCols>`.
func ParseDatasets(host, out string) ([]Dataset, error) {
	var dss []Dataset
	for _, line := range nonEmptyLines(out) {
		f, err := fields(line, 20)
		if err != nil {
			return nil, err
		}
		d := Dataset{Host: host, Name: f[0], Type: f[1], GUID: str(f[2])}
		ints := []struct {
			dst *int64
			val string
		}{
			{&d.Used, f[3]}, {&d.Avail, f[4]}, {&d.Refer, f[5]},
			{&d.Creation, f[6]}, {&d.CreateTXG, f[7]}, {&d.Written, f[8]},
			{&d.RecordSize, f[19]},
		}
		for _, x := range ints {
			if *x.dst, err = num(x.val); err != nil {
				return nil, fmt.Errorf("dataset %q: %w", d.Name, err)
			}
		}
		d.Origin = str(f[9])
		d.Encryption = str(f[10])
		d.KeyStatus = str(f[11])
		d.KeyLocation = str(f[12])
		d.Mounted = boolish(f[13])
		d.CanMount = str(f[14])
		d.Mountpoint = str(f[15])
		d.ReadOnly = boolish(f[16])
		d.ResumeToken = str(f[17])
		d.Compression = str(f[18])
		dss = append(dss, d)
	}
	return dss, nil
}

// ParseSnapshots parses `zfs list -H -p -t snapshot -o <SnapshotCols>`.
func ParseSnapshots(host, out string) ([]Snapshot, error) {
	var snaps []Snapshot
	for _, line := range nonEmptyLines(out) {
		f, err := fields(line, 5)
		if err != nil {
			return nil, err
		}
		ds, name, ok := strings.Cut(f[0], "@")
		if !ok {
			return nil, fmt.Errorf("snapshot name without '@': %q", f[0])
		}
		s := Snapshot{Host: host, Dataset: ds, Name: name, GUID: str(f[1])}
		var err1, err2, err3 error
		s.Used, err1 = num(f[2])
		s.Creation, err2 = num(f[3])
		s.CreateTXG, err3 = num(f[4])
		for _, err := range []error{err1, err2, err3} {
			if err != nil {
				return nil, fmt.Errorf("snapshot %q: %w", f[0], err)
			}
		}
		snaps = append(snaps, s)
	}
	return snaps, nil
}

// Section markers used to batch all discovery commands into one ssh round trip.
const (
	MarkPools     = "===ZFSTUI:pools==="
	MarkDatasets  = "===ZFSTUI:datasets==="
	MarkSnapshots = "===ZFSTUI:snapshots==="
	MarkJobs      = "===ZFSTUI:jobs==="
	MarkRC        = "===ZFSTUI:rc=" // followed by "<n>==="
)

// jobsProbe dumps every /tmp/zfstui-job-* dir: status, meta, last progress
// line and expected size. Also used standalone for live progress polling.
const jobsProbe = `for d in /tmp/zfstui-job-*; do [ -d "$d" ] || continue; ` +
	`echo "==J== $d"; head -n1 "$d/status" 2>/dev/null; ` +
	`echo "==M=="; cat "$d/meta.json" 2>/dev/null; echo; ` +
	`echo "==P=="; tail -n1 "$d/progress" 2>/dev/null; echo; ` +
	`echo "==E=="; head -n1 "$d/expected" 2>/dev/null; echo; done; true`

// DiscoverCommand returns the shell command run on each host during refresh.
// Each section echoes its marker, runs one command, and records its exit code
// so partial failures (e.g. no zpool binary) are attributable.
func DiscoverCommand(sudo bool) string {
	pfx := ""
	if sudo {
		pfx = "sudo -n "
	}
	section := func(mark, cmd string) string {
		return fmt.Sprintf("echo '%s'; %s%s; echo '%s'$?'==='", mark, pfx, cmd, MarkRC)
	}
	return strings.Join([]string{
		section(MarkPools, "zpool list -H -p -o "+PoolCols),
		section(MarkDatasets, "zfs list -H -p -t filesystem,volume -o "+DatasetCols),
		section(MarkSnapshots, "zfs list -H -p -t snapshot -o "+SnapshotCols),
		// Jobs need no sudo: dirs are owned by the ssh user that made them.
		fmt.Sprintf("echo '%s'; %s; echo '%s'$?'==='", MarkJobs, jobsProbe, MarkRC),
	}, "; ")
}

// ParseDiscovery splits combined DiscoverCommand output and parses each section.
func ParseDiscovery(host, out string) (HostState, error) {
	st := HostState{Host: host}
	sections := map[string]*strings.Builder{}
	var cur *strings.Builder
	curMark := ""
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == MarkPools || trimmed == MarkDatasets || trimmed == MarkSnapshots || trimmed == MarkJobs:
			curMark = trimmed
			cur = &strings.Builder{}
			sections[curMark] = cur
		case strings.HasPrefix(trimmed, MarkRC):
			rc := strings.TrimSuffix(strings.TrimPrefix(trimmed, MarkRC), "===")
			if rc != "0" {
				return st, fmt.Errorf("%s: discovery section %s exited with %s", host, curMark, rc)
			}
			cur = nil
		case cur != nil:
			cur.WriteString(line)
			cur.WriteString("\n")
		}
	}
	for _, mark := range []string{MarkPools, MarkDatasets, MarkSnapshots, MarkJobs} {
		if sections[mark] == nil {
			return st, fmt.Errorf("%s: discovery output missing section %s", host, mark)
		}
	}
	var err error
	if st.Pools, err = ParsePools(host, sections[MarkPools].String()); err != nil {
		return st, err
	}
	if st.Datasets, err = ParseDatasets(host, sections[MarkDatasets].String()); err != nil {
		return st, err
	}
	if st.Snapshots, err = ParseSnapshots(host, sections[MarkSnapshots].String()); err != nil {
		return st, err
	}
	st.Jobs = ParseJobs(host, sections[MarkJobs].String())
	return st, nil
}

// ParseJobs parses jobsProbe output. Malformed entries are skipped — job
// dirs may be mid-write or owned by another user.
func ParseJobs(host, out string) []JobInfo {
	var jobs []JobInfo
	var cur *JobInfo
	mode := ""
	var metaBuf strings.Builder
	flush := func() {
		if cur == nil {
			return
		}
		cur.Meta = map[string]string{}
		_ = json.Unmarshal([]byte(metaBuf.String()), &cur.Meta)
		if k := cur.Meta["kind"]; k != "" {
			cur.Kind = k
		}
		if cur.State == "" {
			cur.State = "unknown"
		}
		jobs = append(jobs, *cur)
		cur = nil
		metaBuf.Reset()
	}
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "==J== "):
			flush()
			dir := strings.TrimSpace(strings.TrimPrefix(line, "==J== "))
			cur = &JobInfo{Host: host, Dir: dir, ID: strings.TrimPrefix(dir, jobDirPrefix)}
			mode = "status"
		case line == "==M==":
			mode = "meta"
		case line == "==P==":
			mode = "progress"
		case line == "==E==":
			mode = "expected"
		case cur == nil:
			// ignore noise outside entries
		default:
			switch mode {
			case "status":
				if cur.Status == "" && strings.TrimSpace(line) != "" {
					cur.Status = strings.TrimSpace(line)
					cur.State, _, _ = strings.Cut(cur.Status, " ")
				}
			case "meta":
				metaBuf.WriteString(line)
				metaBuf.WriteString("\n")
			case "progress":
				if v, err := strconv.ParseInt(strings.TrimSpace(line), 10, 64); err == nil {
					cur.Progress = v
				}
			case "expected":
				if v, err := strconv.ParseInt(strings.TrimSpace(line), 10, 64); err == nil {
					cur.Expected = v
				}
			}
		}
	}
	flush()
	return jobs
}
