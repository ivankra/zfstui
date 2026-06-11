// All state-mutating remote commands are built here and ONLY here — this is
// the structural enforcement of DESIGN.md "Data safety (hard rules)". Tests
// assert everything built here matches the allowlist. Inputs are validated
// against strict charsets and shell-quoted anyway (belt and suspenders).
package main

import (
	"fmt"
	"regexp"
	"strings"
)

// ZFS snapshot names: conservative subset of what zfs allows.
var snapNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]*$`)

// Dataset names come from inventory (not the user), but validate anyway.
// Spaces are legal in ZFS; shell metacharacters and '@' are not welcome here.
var datasetRe = regexp.MustCompile(`^[A-Za-z0-9_.:-][A-Za-z0-9_.: /-]*$`)

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func validDataset(ds string) error {
	if !datasetRe.MatchString(ds) || strings.Contains(ds, "//") {
		return fmt.Errorf("refusing suspicious dataset name %q", ds)
	}
	return nil
}

func validSnapName(name string) error {
	if !snapNameRe.MatchString(name) {
		return fmt.Errorf("invalid snapshot name %q (allowed: letters, digits, _ . : -)", name)
	}
	return nil
}

// buildSnapshotCmd returns the `zfs snapshot` command line to run on the
// host (sudo-wrapped for non-root access users).
func buildSnapshotCmd(sudo bool, dataset, snap string, recursive bool) (string, error) {
	if err := validDataset(dataset); err != nil {
		return "", err
	}
	if err := validSnapName(snap); err != nil {
		return "", err
	}
	cmd := "zfs snapshot "
	if recursive {
		cmd += "-r "
	}
	cmd += shellQuote(dataset + "@" + snap)
	if sudo {
		cmd = "sudo -n " + cmd
	}
	return cmd, nil
}
