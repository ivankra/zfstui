// zfstui — manage ZFS dataset replication across hosts. See DESIGN.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"
)

const usage = `usage: zfstui [flags] [command]

commands:
  (none)    open the TUI
  refresh   ssh to all hosts and update the local inventory cache
  status    print the groups × host:pool replication matrix from the cache

  (snap, sync, prune, jobs, doctor are not implemented yet — see DESIGN.md)

flags (before the command):
  -config PATH   config file (default ./zfstui.toml while in development)
  -hosts a,b,c   inline inventory: filters configured hosts / adds unknown ones
  -db PATH       sqlite cache (default ./zfstui.db while in development; :memory: works)
  -a             status: also show hidden groups (those with no snapshots at all)

status cell markers:  + unsnapshotted writes  ! diverged  ~ resumable  % clone
`

func main() {
	var (
		configPath = flag.String("config", "", "config file path")
		hostsFlag  = flag.String("hosts", "", "comma-separated inline host list")
		dbPath     = flag.String("db", "", "sqlite cache path")
		showAll    = flag.Bool("a", false, "status: show hidden (snapshot-less) groups too")
	)
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()

	dbp := *dbPath
	if dbp == "" {
		dbp = DefaultDBPath()
	}
	st, err := openStore(dbp)
	if err != nil {
		fatal(err)
	}
	defer st.Close()

	// Only commands that ssh out need an inventory; status reads the cache.
	mustConfig := func() *Config {
		cfg, err := loadConfig(*configPath, splitComma(*hostsFlag))
		if err != nil {
			fatal(err)
		}
		return cfg
	}

	cmd := flag.Arg(0)
	switch cmd {
	case "":
		// TUI: config is optional (without it, refresh keys are disabled).
		cfg, cfgErr := loadConfig(*configPath, splitComma(*hostsFlag))
		if cfgErr != nil {
			cfg = nil
		}
		err = runTUI(st, cfg)
	case "refresh":
		err = cmdRefresh(mustConfig(), st)
	case "status":
		err = cmdStatus(st, *showAll)
	default:
		fmt.Fprintf(os.Stderr, "zfstui: unknown command %q (not implemented yet?)\n\n", cmd)
		flag.Usage()
		os.Exit(2)
	}
	if err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "zfstui:", err)
	os.Exit(1)
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}

func cmdRefresh(cfg *Config, st *Store) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	started := time.Now()
	results := Refresh(ctx, NewSSH(), st, cfg.Hosts)

	ok := 0
	for _, res := range results {
		if res.Err == nil {
			ok++
			fmt.Printf("  %-12s ok\n", res.Host)
		} else {
			fmt.Printf("  %-12s UNREACHABLE: %v\n", res.Host, res.Err)
		}
	}
	fmt.Printf("refresh: %d/%d hosts in %.1fs\n", ok, len(cfg.Hosts), time.Since(started).Seconds())
	if ok == 0 {
		return fmt.Errorf("no hosts reachable")
	}
	return nil
}

func cmdStatus(st *Store, showAll bool) error {
	data, err := loadData(st)
	if err != nil {
		return err
	}
	if len(data.States) == 0 {
		return fmt.Errorf("cache is empty — run `zfstui refresh` first")
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
	fmt.Print(Status(groups))
	if hidden > 0 {
		fmt.Printf("(%d snapshot-less groups hidden; -a shows them)\n", hidden)
	}

	for _, h := range data.Hosts {
		if !h.Reachable {
			fmt.Printf("note: %s unreachable at last refresh (%s): %s\n",
				h.Name, h.LastRefresh.Format(time.DateTime), h.LastError)
		}
	}
	return nil
}
