// zfstui — manage ZFS dataset replication across hosts. See DESIGN.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"
)

const usage = `usage: zfstui [flags] [command] [host-spec...]

commands:
  (none)    open the TUI
  refresh   ssh to hosts and update the local inventory cache
  status    print the groups × host:pool replication matrix from the cache

  (snap, sync, prune, jobs, doctor are not implemented yet — see DESIGN.md)

host specs (the hostname is the identity; the rest is access info, which is
remembered in the cache db and replaced when you pass a new spec for a host):
  host                  ssh as root@host
  user@host[:pool]      scp-style (sudo implied for non-root users)
  ssh://user;send_user=u1;recv_user=u2@host[:port][/pool]

With no specs, all hosts remembered in the cache are used; with specs, the
command is limited to those hosts.

flags (before the command):
  -db PATH   sqlite cache (default ./zfstui.db while in development; :memory: works)
  -a         status: also show hidden groups (those with no snapshots at all)

status cell markers:  ! diverged  % clone
  (ahead/resumable/unsnapshotted-writes carry no marker — color-only in the TUI)
`

func main() {
	var (
		dbPath  = flag.String("db", "", "sqlite cache path")
		showAll = flag.Bool("a", false, "status: show hidden (snapshot-less) groups too")
	)
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()

	dbp := *dbPath
	if dbp == "" {
		dbp = defaultDBPath()
	}
	st, err := openStore(dbp)
	if err != nil {
		fatal(err)
	}
	defer st.Close()

	// First positional arg is the command if it matches one; the rest
	// (or everything, for the TUI) are host specs.
	args := flag.Args()
	cmd := ""
	if len(args) > 0 && (args[0] == "refresh" || args[0] == "status") {
		cmd = args[0]
		args = args[1:]
	}

	switch cmd {
	case "":
		var hosts []Host
		if hosts, err = resolveHosts(st, args); err == nil {
			err = runTUI(st, hosts)
		}
	case "refresh":
		var hosts []Host
		if hosts, err = resolveHosts(st, args); err == nil {
			err = cmdRefresh(hosts, st)
		}
	case "status":
		err = cmdStatus(st, *showAll, args)
	}
	if err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "zfstui:", err)
	os.Exit(1)
}

func cmdRefresh(hosts []Host, st *Store) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	started := time.Now()
	results := Refresh(ctx, newSSH(), st, hosts)

	ok := 0
	for _, res := range results {
		if res.Err == nil {
			ok++
			fmt.Printf("  %-12s ok\n", res.Host)
		} else {
			fmt.Printf("  %-12s UNREACHABLE: %v\n", res.Host, res.Err)
		}
	}
	fmt.Printf("refresh: %d/%d hosts in %.1fs\n", ok, len(hosts), time.Since(started).Seconds())
	if ok == 0 {
		return fmt.Errorf("no hosts reachable")
	}
	return nil
}

func cmdStatus(st *Store, showAll bool, specs []string) error {
	// Specs limit the view (and update remembered access info as usual).
	var only []string
	if len(specs) > 0 {
		hosts, err := resolveHosts(st, specs)
		if err != nil {
			return err
		}
		for _, h := range hosts {
			only = append(only, h.Name)
		}
	}
	data, err := loadData(st, only)
	if err != nil {
		return err
	}
	if len(data.States) == 0 {
		return fmt.Errorf("cache is empty — run `zfstui refresh host...` first")
	}
	groups := data.Groups
	hidden := 0
	if !showAll {
		var visible []*Group
		for _, g := range groups {
			if g.Hidden {
				hidden++
			} else {
				visible = append(visible, g)
			}
		}
		groups = visible
	}
	fmt.Print(Status(data.States, groups))
	if hidden > 0 {
		fmt.Printf("(%d snapshot-less groups hidden; -a shows them)\n", hidden)
	}

	for _, h := range data.Hosts {
		if !h.Reachable {
			fmt.Printf("note: %s unreachable at last refresh (%s): %s\n",
				h.Name, utcStamp(h.LastRefresh), h.LastError)
		}
	}
	return nil
}
