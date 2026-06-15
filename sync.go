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

// syncJob builds the job that runs DETACHED ON THE SOURCE HOST: zfs send
// piped (through mbuffer when available) to zfs recv — over ssh for remote
// destinations, a plain pipe for pool-to-pool on the same host.
//
// Remote destinations use a per-job ssh ControlMaster: the master
// authenticates while the control node's forwarded agent (-A at launch) is
// still alive, then `touch ssh-ok` tells the launcher it may disconnect;
// the pipeline rides the master afterwards, no agent needed.
func syncJob(srcHost, destHost Host, src *Replica, p syncPlan, intermediate bool) (jobSpec, error) {
	if !p.Selectable() {
		return jobSpec{}, fmt.Errorf("plan for %s is not executable: %s", p.DestLabel(), p.Reason)
	}
	var sendCmd string
	var err error
	raw := src.Dataset.Encrypted()
	switch p.Kind {
	case planResume:
		sendCmd, err = buildResumeSendCmd(srcHost.NeedSudo(), p.Dest.Dataset.ResumeToken)
	case planFull:
		sendCmd, err = buildFullSendCmd(srcHost.NeedSudo(), raw, src.Dataset.Name, p.TargetName)
	default:
		sendCmd, err = buildSendCmd(srcHost.NeedSudo(), raw, intermediate, src.Dataset.Name, p.BaseName, p.TargetName)
	}
	if err != nil {
		return jobSpec{}, err
	}
	// Full send of an unencrypted source: create the new dataset unencrypted
	// (see buildRecvCmd) — otherwise an encrypted destination parent would
	// demand its key loaded just to create the child.
	plainNew := p.Kind == planFull && !raw
	recvCmd, err := buildRecvCmd(destHost.NeedSudo(), p.Dest.Dataset.Name, plainNew)
	if err != nil {
		return jobSpec{}, err
	}

	local := srcHost.Name == destHost.Name
	var b strings.Builder
	b.WriteString(`if command -v mbuffer >/dev/null 2>&1; then ZBUF="mbuffer -q -s 128k -m 128M"; else echo "note: mbuffer not found, sending unbuffered"; ZBUF="cat"; fi` + "\n")
	// Progress instrumentation: expected stream size via a no-op send (-nP),
	// live byte counts via pv into ./progress (2s interval). Optional both.
	fmt.Fprintf(&b, "%s 2>/dev/null | awk '$1==\"size\"{s=$2} END{if(s)print s}' > expected\n", sizeVariant(sendCmd))
	b.WriteString(`if command -v pv >/dev/null 2>&1; then PROG="pv -nb -i 2"; else PROG="cat"; fi` + "\n")
	if local {
		fmt.Fprintf(&b, "%s | $ZBUF | $PROG 2>>progress | %s", sendCmd, recvCmd)
	} else {
		if !destSpecRe.MatchString(destHost.Dest()) {
			return jobSpec{}, fmt.Errorf("refusing suspicious ssh destination %q", destHost.Dest())
		}
		dssh := "ssh -o BatchMode=yes -o ConnectTimeout=15 -o ServerAliveInterval=30" +
			" -o ControlMaster=auto -o ControlPath=$PWD/cm -o ControlPersist=600"
		if destHost.Port != 0 {
			dssh += fmt.Sprintf(" -p %d", destHost.Port)
		}
		dssh += " " + destHost.Dest()
		fmt.Fprintf(&b, "DSSH=%q\n", dssh)
		b.WriteString(`$DSSH true || { echo "cannot reach destination over ssh (no agent/keys? dest down?); SSH_AUTH_SOCK=${SSH_AUTH_SOCK:-unset}"; exit 80; }` + "\n")
		b.WriteString("touch ssh-ok\n")
		fmt.Fprintf(&b, "%s | $ZBUF | $PROG 2>>progress | $DSSH \"%s\"\n", sendCmd, recvCmd)
		b.WriteString("rc=$?\n$DSSH -O exit >/dev/null 2>&1 || true\nexit $rc")
	}

	return jobSpec{
		ID: newJobID(), Kind: "sync", Host: srcHost.Name,
		Command:   b.String(),
		WaitStart: !local,
		Meta: map[string]string{
			"src":    srcHost.Name + ":" + src.Dataset.Name,
			"dest":   p.DestLabel(),
			"base":   p.BaseName,
			"target": p.TargetName,
			"resume": fmt.Sprint(p.Kind == planResume),
			"full":   fmt.Sprint(p.Kind == planFull),
		},
	}, nil
}
