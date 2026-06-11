package main

import (
	"regexp"
	"strings"
	"testing"
)

// The data-safety allowlist (DESIGN.md "Data safety (hard rules)"): every
// mutating command line the tool can emit must match one of these shapes.
var allowlist = regexp.MustCompile(
	`^(sudo -n )?zfs (snapshot( -r)? |send |recv -s -u |receive -A |destroy )'[^;|&$<>\\]*'$`)

func TestSnapshotCmdAllowlist(t *testing.T) {
	for _, c := range []struct {
		sudo      bool
		dataset   string
		snap      string
		recursive bool
	}{
		{false, "tank/srv/git", "20260611", false},
		{true, "tank/srv/git", "20260611", true},
		{true, "tank/bak/blobs/01", "pre-upgrade_2", false},
		{false, "tank/data with space", "a:b.c-d", false},
	} {
		cmd, err := buildSnapshotCmd(c.sudo, c.dataset, c.snap, c.recursive)
		if err != nil {
			t.Errorf("%+v: %v", c, err)
			continue
		}
		if !allowlist.MatchString(cmd) {
			t.Errorf("command outside allowlist: %q", cmd)
		}
	}
}

func TestSnapshotCmdShape(t *testing.T) {
	cmd, err := buildSnapshotCmd(true, "tank/srv/git", "20260611", true)
	if err != nil {
		t.Fatal(err)
	}
	if cmd != `sudo -n zfs snapshot -r 'tank/srv/git@20260611'` {
		t.Errorf("got %q", cmd)
	}
	cmd, _ = buildSnapshotCmd(false, "tank/srv/git", "20260611", false)
	if cmd != `zfs snapshot 'tank/srv/git@20260611'` {
		t.Errorf("got %q", cmd)
	}
}

func TestSnapshotCmdRejectsBadInput(t *testing.T) {
	bad := []struct{ dataset, snap string }{
		{"tank/srv/git", ""},
		{"tank/srv/git", "with space"},
		{"tank/srv/git", "$(boom)"},
		{"tank/srv/git", "a;b"},
		{"tank/srv/git", "-leadingdash"},
		{"tank/srv/git", "name\nnewline"},
		{"tank;rm -rf /", "ok"},
		{"tank/`boom`", "ok"},
		{"tank//x", "ok"},
		{"", "ok"},
	}
	for _, c := range bad {
		if _, err := buildSnapshotCmd(false, c.dataset, c.snap, false); err == nil {
			t.Errorf("accepted bad input %+v", c)
		}
	}
}

func TestShellQuote(t *testing.T) {
	if q := shellQuote(`it's`); q != `'it'\''s'` {
		t.Errorf("got %q", q)
	}
	if q := shellQuote("plain"); q != "'plain'" {
		t.Errorf("got %q", q)
	}
}

func TestQuotedPayloadNeverBreaksOut(t *testing.T) {
	// Even if validation were bypassed, quoting must contain the payload.
	q := shellQuote(`a'; rm -rf / #`)
	if strings.Contains(q, `'; rm`) && !strings.Contains(q, `'\''`) {
		t.Errorf("quote escape failed: %q", q)
	}
}
