package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func testSnapshotJob(t *testing.T) jobSpec {
	t.Helper()
	spec, err := snapshotJob(Host{Name: "ws", User: "ivan"}, "tank/srv/git", "20260611", false)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func TestJobIDShape(t *testing.T) {
	id := newJobID()
	if !regexp.MustCompile(`^\d{8}T\d{6}Z-[0-9a-f]{4}$`).MatchString(id) {
		t.Errorf("job id %q", id)
	}
}

func TestJobScriptIsValidBash(t *testing.T) {
	spec := testSnapshotJob(t)
	f := filepath.Join(t.TempDir(), "job.sh")
	if err := os.WriteFile(f, []byte(spec.script()), 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("bash", "-n", f).CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, out)
	}
}

// Structural data-safety check: every mutating line in a generated job
// script must match the allowlist (the command is on the "( cmd ) >> log"
// line; nothing else in the script may invoke zfs/zpool).
func TestJobScriptMutationsAllowlisted(t *testing.T) {
	spec := testSnapshotJob(t)
	for _, line := range strings.Split(spec.script(), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.Contains(trimmed, "zfs ") && !strings.Contains(trimmed, "zpool ") {
			continue
		}
		if strings.HasPrefix(trimmed, "\"command\":") {
			continue // meta.json echo, not executed
		}
		cmd := strings.TrimSuffix(strings.TrimPrefix(trimmed, "( "), " ) >> log 2>&1")
		if !allowlist.MatchString(cmd) {
			t.Errorf("job script contains non-allowlisted mutation: %q", trimmed)
		}
	}
}

func TestJobScriptContents(t *testing.T) {
	spec := testSnapshotJob(t)
	s := spec.script()
	for _, want := range []string{
		"sudo -n zfs snapshot 'tank/srv/git@20260611'", // ivan => sudo
		`"kind": "snapshot"`,
		`"dataset": "tank/srv/git"`,
		"> status",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script missing %q:\n%s", want, s)
		}
	}
	if !strings.HasPrefix(spec.bootstrap(), "d=/tmp/zfstui-job-") {
		t.Errorf("bootstrap: %q", spec.bootstrap())
	}
}

// jobRunner fakes the two ssh round trips of a job: launch and status polls.
type jobRunner struct {
	launchedCmd   string
	launchedStdin string
	statusOut     []string // successive poll outputs
	polls         int
}

func (j *jobRunner) Run(_ context.Context, h Host, cmd string) ([]byte, error) {
	out := j.statusOut[min(j.polls, len(j.statusOut)-1)]
	j.polls++
	return []byte(out), nil
}

func (j *jobRunner) RunInput(_ context.Context, h Host, cmd string, stdin []byte) ([]byte, error) {
	j.launchedCmd = cmd
	j.launchedStdin = string(stdin)
	return []byte("launched " + jobDirPrefix + extractJobID(cmd) + "\n"), nil
}

func extractJobID(bootstrapCmd string) string {
	m := regexp.MustCompile(`/tmp/zfstui-job-(\S+?) `).FindStringSubmatch(bootstrapCmd + " ")
	if m == nil {
		return "???"
	}
	return strings.Trim(m[1], `"&`)
}

func TestLaunchAndWaitJob(t *testing.T) {
	spec := testSnapshotJob(t)
	r := &jobRunner{statusOut: []string{
		"running 1234 t\n==ZFSTUI:LOG==\n",
		"done t\n==ZFSTUI:LOG==\nall good\n",
	}}
	ctx := context.Background()
	h := Host{Name: "ws", User: "ivan"}
	if err := launchJob(ctx, r, h, spec); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.launchedStdin, spec.Command) {
		t.Error("job.sh not transferred via stdin")
	}
	if !strings.Contains(r.launchedCmd, "setsid bash") || !strings.Contains(r.launchedCmd, "mkdir -m 0700") {
		t.Errorf("bootstrap: %q", r.launchedCmd)
	}
	state, logTail, err := waitJob(ctx, r, h, spec.ID, 5*time.Second)
	if err != nil || state != "done" || !strings.Contains(logTail, "all good") {
		t.Errorf("state=%q log=%q err=%v", state, logTail, err)
	}
}

func TestWaitJobFailure(t *testing.T) {
	r := &jobRunner{statusOut: []string{"failed 1 t\n==ZFSTUI:LOG==\ncannot create snapshot\n"}}
	state, logTail, err := waitJob(context.Background(), r, Host{Name: "ws"}, "x", time.Second)
	if err != nil || state != "failed" || !strings.Contains(logTail, "cannot create") {
		t.Errorf("state=%q log=%q err=%v", state, logTail, err)
	}
}
