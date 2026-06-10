package main

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const sampleConfig = `
[defaults]
user = "root"
engine = "raw"

[hosts.dev1]

[hosts.dev2]
user = "ivan"
sudo = true

[hosts.hetzner]
addr = "h1.example.com"
recv_user = "zfsrecv"
`

func TestLoad(t *testing.T) {
	cfg, err := loadConfig(write(t, sampleConfig), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Hosts) != 3 {
		t.Fatalf("got %d hosts", len(cfg.Hosts))
	}
	if cfg.Defaults.Engine != "raw" {
		t.Errorf("engine=%q", cfg.Defaults.Engine)
	}
	// Sorted: dev1, dev2, hetzner.
	d1, d2, hz := cfg.Hosts[0], cfg.Hosts[1], cfg.Hosts[2]
	if d1.Name != "dev1" || d1.User != "root" || d1.Sudo {
		t.Errorf("dev1: %+v", d1)
	}
	if d2.User != "ivan" || !d2.Sudo {
		t.Errorf("dev2: %+v", d2)
	}
	if hz.Dest() != "root@h1.example.com" || hz.RecvUser != "zfsrecv" {
		t.Errorf("hetzner: %+v dest=%q", hz, hz.Dest())
	}
}

func TestInlineFiltersAndAdds(t *testing.T) {
	cfg, err := loadConfig(write(t, sampleConfig), []string{"dev2", "newhost"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Hosts) != 2 {
		t.Fatalf("got %d hosts: %+v", len(cfg.Hosts), cfg.Hosts)
	}
	// dev2 keeps its configured settings; newhost gets defaults.
	if cfg.Hosts[0].Name != "dev2" || !cfg.Hosts[0].Sudo {
		t.Errorf("dev2: %+v", cfg.Hosts[0])
	}
	if cfg.Hosts[1].Name != "newhost" || cfg.Hosts[1].User != "root" {
		t.Errorf("newhost: %+v", cfg.Hosts[1])
	}
}

func TestInlineUserAtHost(t *testing.T) {
	cfg, err := loadConfig(write(t, sampleConfig), []string{"admin@dev2", "root@newhost"})
	if err != nil {
		t.Fatal(err)
	}
	// Name excludes the user; user overrides config/defaults; access unchanged.
	d2 := cfg.Hosts[0]
	if d2.Name != "dev2" || d2.User != "admin" || !d2.Sudo || d2.Dest() != "admin@dev2" {
		t.Errorf("dev2: %+v dest=%q", d2, d2.Dest())
	}
	nh := cfg.Hosts[1]
	if nh.Name != "newhost" || nh.Dest() != "root@newhost" {
		t.Errorf("newhost: %+v dest=%q", nh, nh.Dest())
	}
}

func TestNoConfigInlineOnly(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.toml")
	// Default-path semantics: missing file tolerated when -hosts given...
	cfg, err := loadConfig("", []string{"a", "b"})
	if err == nil && len(cfg.Hosts) != 2 {
		t.Errorf("got %+v", cfg.Hosts)
	}
	// ...but an explicitly named missing file is an error.
	if _, err := loadConfig(missing, []string{"a"}); err == nil {
		t.Error("want error for explicit missing config")
	}
}

func TestNoHostsAtAll(t *testing.T) {
	if _, err := loadConfig(write(t, "[defaults]\n"), nil); err == nil {
		t.Error("want error when config has no hosts")
	}
}
