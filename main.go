// zfstui — manage ZFS dataset replication across hosts. See DESIGN.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"golang.org/x/sync/errgroup"

	"ivank.me/zfstui/internal/config"
	"ivank.me/zfstui/internal/derive"
	"ivank.me/zfstui/internal/model"
	"ivank.me/zfstui/internal/sshx"
	"ivank.me/zfstui/internal/store"
	"ivank.me/zfstui/internal/view"
	"ivank.me/zfstui/internal/zfsout"
)

const usage = `usage: zfstui [flags] <command>

commands:
  refresh   ssh to all hosts and update the local inventory cache
  status    print the groups × host:pool replication matrix from the cache

  (snap, sync, prune, jobs, doctor, and the TUI are not implemented yet
   — see DESIGN.md for the roadmap)

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

	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(2)
	}
	cmd := flag.Arg(0)

	dbp := *dbPath
	if dbp == "" {
		dbp = config.DefaultDBPath()
	}
	st, err := store.Open(dbp)
	if err != nil {
		fatal(err)
	}
	defer st.Close()

	// Only commands that ssh out need an inventory; status reads the cache.
	loadConfig := func() *config.Config {
		cfg, err := config.Load(*configPath, splitComma(*hostsFlag))
		if err != nil {
			fatal(err)
		}
		return cfg
	}

	switch cmd {
	case "refresh":
		err = cmdRefresh(loadConfig(), st)
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

func cmdRefresh(cfg *config.Config, st *store.Store) error {
	runner := sshx.NewSSH()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	started := time.Now()
	var eg errgroup.Group
	eg.SetLimit(8)
	results := make([]error, len(cfg.Hosts))
	for i, h := range cfg.Hosts {
		eg.Go(func() error {
			results[i] = refreshHost(ctx, runner, st, h)
			return nil
		})
	}
	eg.Wait()

	ok := 0
	for i, h := range cfg.Hosts {
		if results[i] == nil {
			ok++
			fmt.Printf("  %-12s ok\n", h.Name)
		} else {
			fmt.Printf("  %-12s UNREACHABLE: %v\n", h.Name, results[i])
		}
	}
	fmt.Printf("refresh: %d/%d hosts in %.1fs\n", ok, len(cfg.Hosts), time.Since(started).Seconds())
	if ok == 0 {
		return fmt.Errorf("no hosts reachable")
	}
	return nil
}

func refreshHost(ctx context.Context, r sshx.Runner, st *store.Store, h model.Host) error {
	out, err := r.Run(ctx, h, zfsout.DiscoverCommand(h.Sudo))
	if err == nil {
		var hs model.HostState
		if hs, err = zfsout.ParseDiscovery(h.Name, string(out)); err == nil {
			return st.PutHostState(hs)
		}
	}
	if dbErr := st.MarkUnreachable(h.Name, err); dbErr != nil {
		return dbErr
	}
	return err
}

func cmdStatus(st *store.Store, showAll bool) error {
	states, err := st.AllHostStates()
	if err != nil {
		return err
	}
	if len(states) == 0 {
		return fmt.Errorf("cache is empty — run `zfstui refresh` first")
	}
	groups := derive.Groups(states)
	hidden := 0
	if !showAll {
		var visible []*model.Group
		for _, g := range groups {
			if g.Hidden {
				hidden++
			} else {
				visible = append(visible, g)
			}
		}
		groups = visible
	}
	fmt.Print(view.Status(groups))
	if hidden > 0 {
		fmt.Printf("(%d snapshot-less groups hidden; -a shows them)\n", hidden)
	}

	hosts, err := st.Hosts()
	if err != nil {
		return err
	}
	for _, h := range hosts {
		if !h.Reachable {
			fmt.Printf("note: %s unreachable at last refresh (%s): %s\n",
				h.Name, h.LastRefresh.Format(time.DateTime), h.LastError)
		}
	}
	return nil
}
