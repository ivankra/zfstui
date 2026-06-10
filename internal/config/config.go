// Package config loads the TOML inventory and merges inline -hosts flags.
package config

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"ivank.me/zfstui/internal/model"
)

type Defaults struct {
	User   string `toml:"user"`
	Sudo   bool   `toml:"sudo"`
	Engine string `toml:"engine"` // auto | raw | syncoid
}

type hostSection struct {
	Addr     string   `toml:"addr"`
	Port     int      `toml:"port"`
	User     string   `toml:"user"`
	Sudo     *bool    `toml:"sudo"`
	SendUser string   `toml:"send_user"`
	RecvUser string   `toml:"recv_user"`
	SSHOpts  []string `toml:"ssh_opts"`
}

type file struct {
	Defaults Defaults               `toml:"defaults"`
	Hosts    map[string]hostSection `toml:"hosts"`
}

type Config struct {
	Defaults Defaults
	Hosts    []model.Host // sorted by name
	DBPath   string
}

// Defaults are cwd-relative during development so runs are easy to inspect;
// will move back to XDG paths (~/.config, ~/.local/state) before release.

// DefaultPath returns the default config file location.
func DefaultPath() string {
	return "zfstui.toml"
}

// DefaultDBPath returns the default sqlite cache location.
func DefaultDBPath() string {
	return "zfstui.db"
}

// Load reads the config file (missing file is fine if inlineHosts is
// non-empty) and merges inline hosts: names already configured are kept
// as-is, unknown names are added with defaults — ansible-ish add/extend.
// If inlineHosts is non-empty it also acts as a filter on configured hosts.
func Load(path string, inlineHosts []string) (*Config, error) {
	var f file
	explicit := path != ""
	if path == "" {
		path = DefaultPath()
	}
	if _, err := toml.DecodeFile(path, &f); err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("config %s: %w", path, err)
		}
		if explicit || len(inlineHosts) == 0 {
			return nil, fmt.Errorf("config %s: %w (and no -hosts given)", path, err)
		}
	}
	if f.Defaults.Engine == "" {
		f.Defaults.Engine = "auto"
	}

	cfg := &Config{Defaults: f.Defaults, DBPath: DefaultDBPath()}
	mk := func(name string, h hostSection) model.Host {
		sudo := f.Defaults.Sudo
		if h.Sudo != nil {
			sudo = *h.Sudo
		}
		user := h.User
		if user == "" {
			user = f.Defaults.User
		}
		return model.Host{
			Name: name, Addr: h.Addr, Port: h.Port, User: user, Sudo: sudo,
			SendUser: h.SendUser, RecvUser: h.RecvUser, SSHOpts: h.SSHOpts,
		}
	}

	if len(inlineHosts) > 0 {
		for _, spec := range inlineHosts {
			spec = strings.TrimSpace(spec)
			if spec == "" {
				continue
			}
			// "user@host" sets the ssh user; the host's identity (table
			// display, cache key) is just the host name.
			name := spec
			user := ""
			if u, n, ok := strings.Cut(spec, "@"); ok && u != "" && n != "" {
				user, name = u, n
			}
			h := mk(name, f.Hosts[name])
			if user != "" {
				h.User = user
			}
			cfg.Hosts = append(cfg.Hosts, h)
		}
	} else {
		for name, h := range f.Hosts {
			cfg.Hosts = append(cfg.Hosts, mk(name, h))
		}
	}
	if len(cfg.Hosts) == 0 {
		return nil, fmt.Errorf("no hosts: configure [hosts.<name>] in %s or pass -hosts", path)
	}
	sort.Slice(cfg.Hosts, func(i, j int) bool { return cfg.Hosts[i].Name < cfg.Hosts[j].Name })
	return cfg, nil
}
