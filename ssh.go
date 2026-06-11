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
)

// Runner abstracts remote execution so refresh logic is testable offline.
type Runner interface {
	// Run executes cmd on host and returns its stdout.
	Run(ctx context.Context, host Host, cmd string) ([]byte, error)
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
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=" + strconv.Itoa(s.ConnectTimeoutSec),
	}
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
	args = append(args, extra...)
	args = append(args, host.Dest(), cmd)
	return args
}

func (s *SSH) Run(ctx context.Context, host Host, cmd string) ([]byte, error) {
	c := exec.CommandContext(ctx, "ssh", s.Args(host, nil, cmd)...)
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		msg := bytes.TrimSpace(stderr.Bytes())
		return stdout.Bytes(), fmt.Errorf("ssh %s: %w: %s", host.Name, err, msg)
	}
	return stdout.Bytes(), nil
}
