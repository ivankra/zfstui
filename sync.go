// Sync planning and job construction — the core competency (DESIGN.md "Sync
// planning"). planSync is a pure function: given a source replica and its
// group, it classifies every other member through the compatibility gate.
// Only non-destructive receives are ever planned; everything else is skipped
// with an explicit reason.
package main

import (
	"fmt"
	"regexp"
	"strings"
)

type planKind int

const (
	planIncremental  planKind = iota // clean incremental from common base
	planResume                       // interrupted receive, send -t token
	planFull                         // full send to a new (custom) destination
	planUpToDate                     // nothing to send
	planIncompatible                 // would require a destructive receive — never planned
)

type syncPlan struct {
	Dest   *Replica
	Kind   planKind
	Reason string // human explanation for resume/up-to-date/incompatible
	// BaseName is the SOURCE-side snapshot name of the common base (names
	// may differ across replicas; GUIDs are the identity, but `zfs send -I`
	// needs the source's name for it).
	BaseName   string
	TargetName string // source's latest snapshot name
}

// Selectable reports whether this plan can be executed.
func (p syncPlan) Selectable() bool {
	return p.Kind == planIncremental || p.Kind == planResume || p.Kind == planFull
}

func (p syncPlan) DestLabel() string {
	return p.Dest.Dataset.Host + ":" + p.Dest.Dataset.Name
}

// planSync computes a plan for every other member of the group.
func planSync(src *Replica, g *Group, snapsBy map[string][]Snapshot) ([]syncPlan, error) {
	srcSnaps := snapsBy[src.Dataset.Host+":"+src.Dataset.Name]
	if len(srcSnaps) == 0 {
		return nil, fmt.Errorf("source has no snapshots — take one first (s)")
	}
	target := srcSnaps[len(srcSnaps)-1]
	srcByGUID := make(map[string]string, len(srcSnaps)) // guid -> source-side name
	for _, s := range srcSnaps {
		srcByGUID[s.GUID] = s.Name
	}

	var plans []syncPlan
	for _, dst := range g.Members {
		if dst == src {
			continue
		}
		p := syncPlan{Dest: dst, TargetName: target.Name}
		d := dst.Dataset
		dstSnaps := snapsBy[d.Host+":"+d.Name]
		switch {
		case d.Origin != "":
			p.Kind, p.Reason = planIncompatible, "clone — syncing to clones not supported"
		case d.ResumeToken != "":
			p.Kind, p.Reason = planResume, "resume interrupted receive"
		case len(dstSnaps) == 0:
			p.Kind, p.Reason = planIncompatible, "no snapshots on destination (full send not supported yet)"
		default:
			dstLatest := dstSnaps[len(dstSnaps)-1]
			baseName, shared := srcByGUID[dstLatest.GUID]
			switch {
			case !shared:
				p.Kind = planIncompatible
				p.Reason = fmt.Sprintf("diverged: destination's latest @%s is not on source", dstLatest.Name)
			case d.Written > 0:
				p.Kind = planIncompatible
				p.Reason = fmt.Sprintf("destination has %s of local writes (would need rollback — fix manually)",
					humanShort(d.Written))
			case dstLatest.GUID == target.GUID:
				p.Kind, p.Reason = planUpToDate, "up to date @"+dstLatest.Name
			default:
				p.Kind = planIncremental
				p.BaseName = baseName
				p.Reason = fmt.Sprintf("@%s ⇒ @%s", baseName, target.Name)
			}
		}
		plans = append(plans, p)
	}
	return plans, nil
}

// customDestPlan plans a FULL send of the source's latest snapshot to a
// dataset that does not exist yet (anywhere — typically a new host or pool).
func customDestPlan(src *Replica, snapsBy map[string][]Snapshot, destHost, destDS string) (syncPlan, error) {
	srcSnaps := snapsBy[src.Dataset.Host+":"+src.Dataset.Name]
	if len(srcSnaps) == 0 {
		return syncPlan{}, fmt.Errorf("source has no snapshots — take one first (s)")
	}
	if err := validDataset(destDS); err != nil {
		return syncPlan{}, err
	}
	return syncPlan{
		Dest:       &Replica{Dataset: &Dataset{Host: destHost, Name: destDS, Type: "filesystem"}},
		Kind:       planFull,
		Reason:     "full send (new dataset)",
		TargetName: srcSnaps[len(srcSnaps)-1].Name,
	}, nil
}

var destSpecRe = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9._-]+$`)

// syncJob builds the job that runs DETACHED ON runHost: zfs send piped
// (through mbuffer when available) to zfs recv. Each end runs directly when
// it IS runHost, otherwise over ssh from runHost:
//
//	runHost = source       push:  send | ssh dst recv          (the default)
//	runHost = destination  pull:  ssh src send | recv
//	anything else          relay: ssh src send | ssh dst recv  (incl. @local)
//
// Remote ends use per-job ssh ControlMasters (cm-src, cm-dst): they
// authenticate while the launcher's forwarded agent (-A at launch; a local
// runner simply has the user's agent) is still alive, then `touch ssh-ok`
// tells the launcher it may disconnect; the pipeline rides the masters.
func syncJob(srcHost, destHost, runHost Host, src *Replica, p syncPlan, intermediate bool) (jobSpec, error) {
	if !p.Selectable() {
		return jobSpec{}, fmt.Errorf("plan for %s is not executable: %s", p.DestLabel(), p.Reason)
	}
	prelude, sendCmd, recvCmd, err := syncCmds(srcHost, destHost, src, p, intermediate)
	if err != nil {
		return jobSpec{}, err
	}
	sendRemote := runHost.Name != srcHost.Name
	recvRemote := runHost.Name != destHost.Name
	var sssh, dssh string
	if sendRemote {
		if sssh, err = jobSSH(srcHost, "cm-src"); err != nil {
			return jobSpec{}, err
		}
	}
	if recvRemote {
		if dssh, err = jobSSH(destHost, "cm-dst"); err != nil {
			return jobSpec{}, err
		}
	}
	sendPart, sizePart, recvPart := sendCmd, sizeVariant(sendCmd), recvCmd
	if sendRemote {
		sendPart = `$SSSH "` + sendCmd + `"`
		sizePart = `$SSSH "` + sizeVariant(sendCmd) + `"`
	}
	if recvRemote {
		recvPart = `$DSSH "` + recvCmd + `"`
	}

	// The script is shown in the job window: keep lines short (wrapped
	// blocks, `\`-continued pipes, shared $SSHO, long tokens in $TOKEN).
	var b strings.Builder
	if prelude != "" {
		b.WriteString(prelude + "\n")
	}
	b.WriteString("if command -v mbuffer >/dev/null 2>&1; then\n" +
		"  ZBUF=\"mbuffer -q -s 128k -m 128M\"\n" +
		"else\n" +
		"  echo \"note: mbuffer not found, sending unbuffered\"\n" +
		"  ZBUF=\"cat\"\n" +
		"fi\n")
	if sendRemote || recvRemote {
		fmt.Fprintf(&b, "SSHO=%q\n", jobSSHOpts)
	}
	// ssh checks first: their stderr (the real reason: DNS/timeout/auth)
	// lands in ./log, the echo says which leg failed and from where.
	sshCheck := func(v, what, dest string, rc int) {
		fmt.Fprintf(&b, "$%s true || {\n"+
			"  echo \"cannot reach %s %s over ssh from $(hostname)\"\n"+
			"  echo \"(no agent/keys? DNS? down?) SSH_AUTH_SOCK=${SSH_AUTH_SOCK:-unset}\"\n"+
			"  exit %d\n"+
			"}\n", v, what, dest, rc)
	}
	if sendRemote {
		fmt.Fprintf(&b, "SSSH=%q\n", sssh)
		sshCheck("SSSH", "source", srcHost.Dest(), 80)
	}
	if recvRemote {
		fmt.Fprintf(&b, "DSSH=%q\n", dssh)
		sshCheck("DSSH", "destination", destHost.Dest(), 81)
	}
	if sendRemote || recvRemote {
		b.WriteString("touch ssh-ok\n")
	}
	// Progress instrumentation: expected stream size via a no-op send (-nP),
	// live byte counts via pv into ./progress (2s interval). Optional both.
	fmt.Fprintf(&b, "%s 2>/dev/null \\\n  | awk '$1==\"size\"{s=$2} END{if(s)print s}' > expected\n", sizePart)
	b.WriteString("if command -v pv >/dev/null 2>&1; then\n" +
		"  PROG=\"pv -nb -i 2\"\n" +
		"else\n" +
		"  PROG=\"cat\"\n" +
		"fi\n")
	fmt.Fprintf(&b, "%s \\\n  | $ZBUF | $PROG 2>>progress \\\n  | %s\n", sendPart, recvPart)
	b.WriteString("rc=$?\n")
	if sendRemote {
		b.WriteString("$SSSH -O exit >/dev/null 2>&1 || true\n")
	}
	if recvRemote {
		b.WriteString("$DSSH -O exit >/dev/null 2>&1 || true\n")
	}
	b.WriteString("exit $rc")

	return jobSpec{
		ID: newJobID(), Kind: "sync", Host: runHost.Name,
		Command:   b.String(),
		WaitStart: sendRemote || recvRemote,
		Meta: map[string]string{
			"src":          srcHost.Name + ":" + src.Dataset.Name,
			"dest":         p.DestLabel(),
			"runner":       runHost.Name,
			"base":         p.BaseName,
			"target":       p.TargetName,
			"resume":       fmt.Sprint(p.Kind == planResume),
			"full":         fmt.Sprint(p.Kind == planFull),
			"intermediate": fmt.Sprint(intermediate),
			"manual":       manualSyncCmd(srcHost, destHost, runHost, prelude, sendCmd, recvCmd),
		},
	}, nil
}

// syncCmds builds the bare send and recv command lines (mutate.go) for a
// plan. prelude (resume only: `TOKEN='…'`) must precede them in the shell
// that runs the pipeline.
func syncCmds(srcHost, destHost Host, src *Replica, p syncPlan, intermediate bool) (prelude, sendCmd, recvCmd string, err error) {
	raw := src.Dataset.Encrypted()
	switch p.Kind {
	case planResume:
		prelude, sendCmd, err = buildResumeSendCmd(srcHost.NeedSudo(), p.Dest.Dataset.ResumeToken)
	case planFull:
		sendCmd, err = buildFullSendCmd(srcHost.NeedSudo(), raw, src.Dataset.Name, p.TargetName)
	default:
		sendCmd, err = buildSendCmd(srcHost.NeedSudo(), raw, intermediate, src.Dataset.Name, p.BaseName, p.TargetName)
	}
	if err != nil {
		return "", "", "", err
	}
	// Full send of an unencrypted source: create the new dataset unencrypted
	// (see buildRecvCmd) — otherwise an encrypted destination parent would
	// demand its key loaded just to create the child.
	plainNew := p.Kind == planFull && !raw
	recvCmd, err = buildRecvCmd(destHost.NeedSudo(), p.Dest.Dataset.Name, plainNew)
	return prelude, sendCmd, recvCmd, err
}

// jobSSHOpts are the ssh options shared by a job's remote legs; the script
// holds them in $SSHO (expanded when SSSH/DSSH are assigned).
const jobSSHOpts = "-o BatchMode=yes -o ConnectTimeout=15 -o ServerAliveInterval=30" +
	" -o ControlMaster=auto -o ControlPersist=600"

// jobSSH is the ssh command a job uses to reach h, multiplexed over a
// per-job ControlMaster socket in the job dir. Needs $SSHO set first.
func jobSSH(h Host, socket string) (string, error) {
	if !destSpecRe.MatchString(h.Dest()) {
		return "", fmt.Errorf("refusing suspicious ssh destination %q", h.Dest())
	}
	s := "ssh $SSHO -o ControlPath=$PWD/" + socket
	if h.Port != 0 {
		s += fmt.Sprintf(" -p %d", h.Port)
	}
	return s + " " + h.Dest(), nil
}

// plainSSH is the human-readable ssh prefix for the manual command.
func plainSSH(h Host) string {
	if h.Port != 0 {
		return fmt.Sprintf("ssh -p %d %s", h.Port, h.Dest())
	}
	return "ssh " + h.Dest()
}

// manualSyncCmd renders the essence of the job for humans: the bare
// send | recv pipeline as run on runHost (no buffering/progress/defensive
// plumbing), plus — unless that already IS it — the same transfer relayed
// from the user's own terminal, which works whenever zfstui reaches both.
// Display only (job window "manual" section); never executed by zfstui.
// prelude (`TOKEN='…'`, may be empty) heads each block: each runs in its own
// shell.
func manualSyncCmd(srcHost, destHost, runHost Host, prelude, sendCmd, recvCmd string) string {
	leg := func(h Host, cmd string, remote bool) string {
		if !remote {
			return cmd
		}
		return plainSSH(h) + ` "` + cmd + `"`
	}
	pipe := func(run string) string {
		out := leg(srcHost, sendCmd, run != srcHost.Name) + " \\\n  | " + leg(destHost, recvCmd, run != destHost.Name)
		if prelude != "" {
			out = prelude + "\n" + out
		}
		return out
	}
	out := "# on " + displayHost(runHost.Name) + ":\n" + pipe(runHost.Name)
	if runHost.Name != localHostName {
		out += "\n# or relayed from your own terminal:\n" + pipe(localHostName)
	}
	return out
}
