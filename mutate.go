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

// sendFlags picks stream flags by source encryption. Encrypted: -w (raw,
// required for key-less backups). Unencrypted: -L -c — NOT -w, because for
// unencrypted datasets -w implies -e (embedded blocks) and such streams
// "may not be received as an encrypted dataset" (zfs-send(8)), which breaks
// unencrypted→encrypted-parent syncs ("inherited key must be loaded").
func sendFlags(raw bool) string {
	if raw {
		return "-w"
	}
	return "-L -c"
}

// buildSendCmd returns the incremental `zfs send` command (runs on the
// source host). -I sends all intermediate snapshots, -i only the delta.
func buildSendCmd(sudo, raw, intermediate bool, dataset, baseSnap, targetSnap string) (string, error) {
	if err := validDataset(dataset); err != nil {
		return "", err
	}
	if err := validSnapName(baseSnap); err != nil {
		return "", err
	}
	if err := validSnapName(targetSnap); err != nil {
		return "", err
	}
	flag := "-i"
	if intermediate {
		flag = "-I"
	}
	cmd := fmt.Sprintf("zfs send %s %s %s %s", sendFlags(raw), flag,
		shellQuote(dataset+"@"+baseSnap), shellQuote(dataset+"@"+targetSnap))
	if sudo {
		cmd = "sudo -n " + cmd
	}
	return cmd, nil
}

// buildFullSendCmd returns a full (non-incremental) `zfs send` of one
// snapshot — used for new destinations that don't have the dataset yet.
func buildFullSendCmd(sudo, raw bool, dataset, targetSnap string) (string, error) {
	if err := validDataset(dataset); err != nil {
		return "", err
	}
	if err := validSnapName(targetSnap); err != nil {
		return "", err
	}
	cmd := "zfs send " + sendFlags(raw) + " " + shellQuote(dataset+"@"+targetSnap)
	if sudo {
		cmd = "sudo -n " + cmd
	}
	return cmd, nil
}

// Resume tokens are opaque dash-separated alphanumerics.
var resumeTokenRe = regexp.MustCompile(`^[A-Za-z0-9-]{8,}$`)

// buildResumeSendCmd resumes an interrupted receive (zfs send -t).
func buildResumeSendCmd(sudo bool, token string) (string, error) {
	if !resumeTokenRe.MatchString(token) {
		return "", fmt.Errorf("refusing suspicious resume token %q", token)
	}
	cmd := "zfs send -t " + shellQuote(token)
	if sudo {
		cmd = "sudo -n " + cmd
	}
	return cmd, nil
}

// buildRecvCmd returns the receive side: resumable (-s), never auto-mounted
// (-u), and NEVER -F (DESIGN "Data safety"). plainNew forces the NEW dataset
// to be created unencrypted (-o encryption=off): used for full sends of
// unencrypted sources, so receiving under an encrypted parent neither
// requires the parent's key to be loaded nor silently encrypts — the backup
// matches the source. Never used for incrementals (encryption is fixed at
// creation) or raw sends (they carry their own keys).
func buildRecvCmd(sudo bool, dataset string, plainNew bool) (string, error) {
	if err := validDataset(dataset); err != nil {
		return "", err
	}
	cmd := "zfs recv -s -u "
	if plainNew {
		cmd += "-o encryption=off "
	}
	cmd += shellQuote(dataset)
	if sudo {
		cmd = "sudo -n " + cmd
	}
	return cmd, nil
}

// sizeVariant turns a send command built above into its no-op (-nP) sizing
// twin — read-only: -n never generates stream data.
func sizeVariant(sendCmd string) string {
	return strings.Replace(sendCmd, "zfs send ", "zfs send -nP ", 1)
}
