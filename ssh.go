// Package sshx runs commands on hosts by shelling out to the system ssh
// binary, so the user's ~/.ssh/config, agent, ProxyJump etc. all apply.
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Runner abstracts remote execution so refresh/job logic is testable offline.
type Runner interface {
	// Run executes cmd on host and returns its stdout.
	Run(ctx context.Context, host Host, cmd string) ([]byte, error)
	// RunInput is Run with data piped to the remote command's stdin (used
	// to transfer job scripts); sshExtra adds ssh flags (e.g. -A).
	RunInput(ctx context.Context, host Host, cmd string, stdin []byte, sshExtra ...string) ([]byte, error)
}

// SSH is the real Runner. ControlMaster multiplexing makes repeated calls to
// the same host cheap (refresh, then job polling).
type SSH struct {
	ControlDir        string // for ControlPath sockets; empty disables multiplexing
	ConnectTimeoutSec int
}

func newSSH() *SSH {
	dir := ""
	home, err := os.UserHomeDir()
	if err == nil {
		dir = filepath.Join(home, ".local", "state", "zfstui", "cm")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			dir = ""
		}
	}
	return &SSH{ControlDir: dir, ConnectTimeoutSec: 8}
}

// Args builds the full ssh argv for running cmd on host (exposed for tests
// and for job-launch code that needs -A etc. via extra).
func (s *SSH) Args(host Host, extra []string, cmd string) []string {
	// extra goes FIRST: ssh honors the first occurrence of an option, so
	// callers can override multiplexing (e.g. ControlPath=none for agent-
	// forwarded job launches — a reused master ignores -A!).
	args := append([]string{}, extra...)
	args = append(args,
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout="+strconv.Itoa(s.ConnectTimeoutSec),
	)
	if s.ControlDir != "" {
		args = append(args,
			"-o", "ControlMaster=auto",
			"-o", "ControlPath="+filepath.Join(s.ControlDir, "%C"),
			"-o", "ControlPersist=120",
		)
	}
	if host.Port != 0 {
		args = append(args, "-p", strconv.Itoa(host.Port))
	}
	args = append(args, host.Dest(), cmd)
	return args
}

func (s *SSH) Run(ctx context.Context, host Host, cmd string) ([]byte, error) {
	return s.RunInput(ctx, host, cmd, nil)
}

func (s *SSH) RunInput(ctx context.Context, host Host, cmd string, stdin []byte, sshExtra ...string) ([]byte, error) {
	c := exec.CommandContext(ctx, "ssh", s.Args(host, sshExtra, cmd)...)
	if stdin != nil {
		c.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		msg := bytes.TrimSpace(stderr.Bytes())
		return stdout.Bytes(), fmt.Errorf("ssh %s: %w: %s", host.Name, err, msg)
	}
	return stdout.Bytes(), nil
}

// localHostName is the pseudo-host for jobs run on THIS machine (sync relay
// through the control node). Not a valid hostname, so it can never collide
// with a fleet host; its Runner is plain local bash, no ssh.
const localHostName = "@local"

// displayHost is how a job host is shown: the local pseudo-host reads as
// plain "localhost" (it can't BE that name internally — a fleet host may be
// called localhost, e.g. via ssh to itself).
func displayHost(name string) string {
	if name == localHostName {
		return "localhost"
	}
	return name
}

// bashRunner runs commands locally through bash (the local job runner).
type bashRunner struct{}

func (bashRunner) Run(ctx context.Context, host Host, cmd string) ([]byte, error) {
	return bashRunner{}.RunInput(ctx, host, cmd, nil)
}

func (bashRunner) RunInput(ctx context.Context, _ Host, cmd string, stdin []byte, _ ...string) ([]byte, error) {
	c := exec.CommandContext(ctx, "bash", "-c", cmd)
	if stdin != nil {
		c.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("local bash: %w: %s", err, bytes.TrimSpace(stderr.Bytes()))
	}
	return stdout.Bytes(), nil
}

// localExec is the Runner for localHostName (a var so tests can stub it —
// they must never see this machine's real /tmp job dirs).
var localExec Runner = bashRunner{}

// localMachineID reads THIS machine's /etc/machine-id ("" if unreadable); a
// var so tests can stub it. Matching a fleet host's discovered id means
// zfstui runs ON that host (see Model.localTwin).
var localMachineID = func() string {
	b, _ := os.ReadFile("/etc/machine-id")
	return strings.TrimSpace(string(b))
}

// runnerFor picks the Runner that reaches h: local bash for localHostName,
// system ssh for everything else.
func runnerFor(h Host) Runner {
	if h.Name == localHostName {
		return localExec
	}
	return newSSH()
}
