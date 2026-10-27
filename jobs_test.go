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
	if !regexp.MustCompile(`^[0-9A-Za-z]{6}$`).MatchString(id) {
		t.Errorf("job id %q", id)
	}
	if id2 := newJobID(); id2 == id {
		t.Errorf("ids should differ: %q", id)
	}
}

// Agent-forwarded launches must be able to defeat ControlMaster reuse: ssh
// honors the FIRST occurrence of an option, so caller extras go first.
func TestSSHArgsExtraFirst(t *testing.T) {
	s := &SSH{ControlDir: "/x", ConnectTimeoutSec: 5}
	args := s.Args(Host{Name: "h"}, []string{"-A", "-o", "ControlPath=none"}, "true")
	if args[0] != "-A" || args[1] != "-o" || args[2] != "ControlPath=none" {
		t.Errorf("extras not first: %v", args)
	}
	found, overrideCount := false, 0
	for _, a := range args[3:] {
		if strings.HasPrefix(a, "ControlPath=") && a != "ControlPath=none" {
			found = true
		}
		if a == "ControlPath=none" {
			overrideCount++
		}
	}
	if !found {
		t.Errorf("multiplexed ControlPath should still follow: %v", args)
	}
	if overrideCount != 0 {
		t.Errorf("extra ControlPath override repeated after defaults: %v", args)
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

// Structural data-safety check: every zfs-invoking segment of a generated
// job script must match the allowlist — including each stage of sync
// pipelines (send | $ZBUF | [$DSSH] recv). Nothing else may invoke zfs/zpool.
func assertScriptAllowlisted(t *testing.T, script string) {
	t.Helper()
	inMeta := false
	for _, line := range strings.Split(strings.ReplaceAll(script, "\\\n", " "), "\n") {
		trimmed := strings.TrimSpace(line)
		// meta.json heredoc (command/manual text): data, not executed.
		if strings.HasPrefix(trimmed, "cat > meta.json <<'ZFSTUI_META'") {
			inMeta = true
			continue
		}
		if inMeta {
			inMeta = trimmed != "ZFSTUI_META"
			continue
		}
		trimmed = strings.TrimSuffix(strings.TrimPrefix(trimmed, "( "), " ) >> log 2>&1")
		if strings.HasPrefix(trimmed, "TOKEN=") && !resumeTokenAssignRe.MatchString(trimmed) {
			t.Errorf("job script contains bad token assignment %q", line)
		}
		if !strings.Contains(trimmed, "zfs ") && !strings.Contains(trimmed, "zpool ") {
			continue
		}
		for _, seg := range strings.Split(trimmed, "|") {
			seg = strings.TrimSpace(seg)
			if !strings.Contains(seg, "zfs ") {
				continue
			}
			// Unwrap the remote forms: $SSSH "send..." / $DSSH "recv..."
			// and strip shell redirections (progress/size plumbing).
			seg = strings.TrimSuffix(seg, " 2>/dev/null")
			seg = strings.TrimPrefix(seg, "$SSSH ")
			seg = strings.TrimPrefix(seg, "$DSSH ")
			seg = strings.Trim(seg, "\"")
			if !allowlisted(seg) {
				t.Errorf("job script contains non-allowlisted mutation: %q (line %q)", seg, line)
			}
		}
	}
	if inMeta {
		t.Error("unterminated meta heredoc")
	}
}

// assertManualAllowlisted checks the human "manual" command the same way:
// every zfs segment (after unwrapping `ssh [-p N] user@host "…"` and the
// `\` line continuations) must be allowlisted.
func assertManualAllowlisted(t *testing.T, manual string) {
	t.Helper()
	sshRe := regexp.MustCompile(`^ssh (-p \d+ )?[A-Za-z0-9._-]+@[A-Za-z0-9._-]+ "(.*)"$`)
	joined := strings.ReplaceAll(manual, "\\\n", " ")
	for _, line := range strings.Split(joined, "\n") {
		if strings.HasPrefix(line, "TOKEN=") && !resumeTokenAssignRe.MatchString(line) {
			t.Errorf("manual command contains bad token assignment %q", line)
		}
		if strings.HasPrefix(line, "#") || !strings.Contains(line, "zfs ") {
			continue
		}
		for _, seg := range strings.Split(line, "|") {
			seg = strings.TrimSpace(seg)
			if m := sshRe.FindStringSubmatch(seg); m != nil {
				seg = m[2]
			}
			if !allowlisted(seg) {
				t.Errorf("manual command contains non-allowlisted segment %q:\n%s", seg, manual)
			}
		}
	}
}

// Tests must never run against this machine's real /tmp job dirs.
func TestMain(m *testing.M) {
	localExec = fakeLocal{}
	localMachineID = func() string { return "" }
	os.Exit(m.Run())
}

type fakeLocal struct{}

func (fakeLocal) Run(context.Context, Host, string) ([]byte, error) { return nil, nil }
func (fakeLocal) RunInput(context.Context, Host, string, []byte, ...string) ([]byte, error) {
	return nil, nil
}

func TestJobScriptMutationsAllowlisted(t *testing.T) {
	assertScriptAllowlisted(t, testSnapshotJob(t).script())
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
	launchedExtra []string
	statusOut     []string // successive poll outputs
	polls         int
}

func (j *jobRunner) Run(_ context.Context, h Host, cmd string) ([]byte, error) {
	out := j.statusOut[min(j.polls, len(j.statusOut)-1)]
	j.polls++
	return []byte(out), nil
}

func (j *jobRunner) RunInput(_ context.Context, h Host, cmd string, stdin []byte, sshExtra ...string) ([]byte, error) {
	j.launchedCmd = cmd
	j.launchedStdin = string(stdin)
	j.launchedExtra = sshExtra
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
