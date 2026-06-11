// Remote job protocol (DESIGN.md "Remote job protocol"): a job is a
// self-describing directory /tmp/zfstui-job-<id>/ on the executing host —
// deliberately /tmp, not a user dir, so discovery of active/finished jobs is
// one glob away and we're not tied to any account layout.
//
//	job.sh     the generated script (the only thing that runs)
//	meta.json  what/why (written by job.sh itself, single file transfer)
//	status     one line: running <pid> <ts> | done <ts> | failed <rc> <ts>
//	log        stdout+stderr of the command
//
// Launch is one ssh round trip: job.sh arrives on stdin, gets detached via
// setsid, and the control node may disconnect immediately.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const jobDirPrefix = "/tmp/zfstui-job-"

type jobSpec struct {
	ID      string
	Kind    string // "snapshot"; later "sync", "prune"
	Host    string
	Command string // single mutation command; ALWAYS built by mutate.go
	Meta    map[string]string
}

func newJobID() string {
	var b [2]byte
	rand.Read(b[:])
	return time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}

func (j jobSpec) Dir() string { return jobDirPrefix + j.ID }

func (j jobSpec) metaJSON() string {
	m := map[string]string{
		"id":      j.ID,
		"kind":    j.Kind,
		"host":    j.Host,
		"command": j.Command,
		"created": time.Now().UTC().Format(time.RFC3339),
	}
	for k, v := range j.Meta {
		m[k] = v
	}
	out, _ := json.MarshalIndent(m, "", "  ")
	return string(out)
}

// script renders job.sh. The command runs in a subshell with output to ./log
// and the status file is written on every exit path.
func (j jobSpec) script() string {
	return fmt.Sprintf(`#!/usr/bin/env bash
# zfstui job %s (%s) — generated; safe to delete when not running
set -u
cd "$(dirname "$0")" || exit 1
cat > meta.json <<'ZFSTUI_META'
%s
ZFSTUI_META
echo "running $$ $(date -u +%%FT%%TZ)" > status
( %s ) >> log 2>&1
rc=$?
if [ "$rc" -eq 0 ]; then
  echo "done $(date -u +%%FT%%TZ)" > status
else
  echo "failed $rc $(date -u +%%FT%%TZ)" > status
fi
exit "$rc"
`, j.ID, j.Kind, j.metaJSON(), j.Command)
}

// bootstrap is the remote command that receives job.sh on stdin and detaches
// it. Prints "launched <dir>" so the caller can verify.
func (j jobSpec) bootstrap() string {
	d := j.Dir()
	return fmt.Sprintf(
		`d=%s && mkdir -m 0700 "$d" && cat > "$d/job.sh" && { setsid bash "$d/job.sh" </dev/null >/dev/null 2>&1 & } && echo "launched $d"`,
		d)
}

// launchJob starts the job on the host and returns once it is detached.
func launchJob(ctx context.Context, r Runner, h Host, j jobSpec) error {
	out, err := r.RunInput(ctx, h, j.bootstrap(), []byte(j.script()))
	if err != nil {
		return fmt.Errorf("launch job %s on %s: %w", j.ID, h.Name, err)
	}
	if !strings.Contains(string(out), "launched "+j.Dir()) {
		return fmt.Errorf("launch job %s on %s: unexpected output %q", j.ID, h.Name, out)
	}
	return nil
}

// waitJob polls the job's status file until done/failed or the timeout
// passes (state "running" then — the job continues without us).
func waitJob(ctx context.Context, r Runner, h Host, id string, timeout time.Duration) (state, logTail string, err error) {
	dir := jobDirPrefix + id
	probe := fmt.Sprintf(`cat %s/status 2>/dev/null; echo '==ZFSTUI:LOG=='; tail -n 20 %s/log 2>/dev/null`, dir, dir)
	deadline := time.Now().Add(timeout)
	for {
		out, err := r.Run(ctx, h, probe)
		if err != nil {
			return "", "", err
		}
		statusLine, logPart, _ := strings.Cut(string(out), "==ZFSTUI:LOG==")
		statusLine = strings.TrimSpace(statusLine)
		logPart = strings.TrimSpace(logPart)
		switch {
		case strings.HasPrefix(statusLine, "done"):
			return "done", logPart, nil
		case strings.HasPrefix(statusLine, "failed"):
			return "failed", logPart, nil
		}
		if time.Now().After(deadline) {
			return "running", logPart, nil
		}
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// snapshotJob builds a snapshot job for one replica.
func snapshotJob(h Host, dataset, snap string, recursive bool) (jobSpec, error) {
	cmd, err := buildSnapshotCmd(h.NeedSudo(), dataset, snap, recursive)
	if err != nil {
		return jobSpec{}, err
	}
	return jobSpec{
		ID: newJobID(), Kind: "snapshot", Host: h.Name, Command: cmd,
		Meta: map[string]string{
			"dataset": dataset, "snapshot": snap, "recursive": fmt.Sprint(recursive),
		},
	}, nil
}
