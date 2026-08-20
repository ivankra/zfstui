package main

import (
	"strings"
	"testing"
)

func TestParseHostSpec(t *testing.T) {
	cases := []struct {
		spec string
		want Host
		url  string // expected reassembled URL ("" = same as canonical root form)
	}{
		{"ws", Host{Name: "ws"}, "root@ws"},
		{"root@ws", Host{Name: "ws"}, "root@ws"}, // root normalizes to default
		{"ivan@dev1", Host{Name: "dev1", User: "ivan"}, "ivan@dev1"},
		{"ivan@dev1:tank", Host{Name: "dev1", User: "ivan", Pool: "tank"}, "ivan@dev1:tank"},
		{"ssh://hetzner", Host{Name: "hetzner"}, "root@hetzner"},
		{"ssh://u@hetzner:2222", Host{Name: "hetzner", User: "u", Port: 2222}, "ssh://u@hetzner:2222"},
		{"ssh://u@hetzner:2222/tank", Host{Name: "hetzner", User: "u", Port: 2222, Pool: "tank"},
			"ssh://u@hetzner:2222/tank"},
		{"ssh://u;send_user=s;recv_user=r@h/p", Host{Name: "h", User: "u", Pool: "p", SendUser: "s", RecvUser: "r"},
			"ssh://u;send_user=s;recv_user=r@h/p"},
	}
	for _, c := range cases {
		h, err := parseHostSpec(c.spec)
		if err != nil {
			t.Errorf("%q: %v", c.spec, err)
			continue
		}
		if h != c.want {
			t.Errorf("%q: got %+v, want %+v", c.spec, h, c.want)
		}
		if got := h.URL(); got != c.url {
			t.Errorf("%q: URL() = %q, want %q", c.spec, got, c.url)
		}
	}
}

func TestParseHostSpecErrors(t *testing.T) {
	for _, spec := range []string{
		"",                      // empty
		"http://ws",             // wrong scheme
		"ssh://u;badopt=1@host", // unknown option
		"ssh://u;noequals@host", // malformed option
		"a@b@c",                 // junk hostname
		"-a",                    // misplaced flag, not a host
		"root@-x",               // leading-dash hostname
	} {
		if _, err := parseHostSpec(spec); err == nil {
			t.Errorf("%q: expected error", spec)
		}
	}
}

func TestSudoImpliedByUser(t *testing.T) {
	root, _ := parseHostSpec("ws")
	if root.NeedSudo() {
		t.Error("root access must not need sudo")
	}
	ivan, _ := parseHostSpec("ivan@ws")
	if !ivan.NeedSudo() {
		t.Error("non-root access must need sudo")
	}
}

func TestParseHostSpecsConflict(t *testing.T) {
	if _, err := parseHostSpecs([]string{"root@ws", "ivan@ws"}); err == nil ||
		!strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("want conflict error, got %v", err)
	}
	// Identical duplicates (even spelled differently) are fine and deduped.
	hosts, err := parseHostSpecs([]string{"ws", "root@ws"})
	if err != nil || len(hosts) != 1 {
		t.Fatalf("got %v, %v", hosts, err)
	}
}

func TestResolveHosts(t *testing.T) {
	st, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Nothing known, no specs: error.
	if _, err := resolveHosts(st, nil); err == nil {
		t.Fatal("want error with empty inventory")
	}

	// Specs are remembered (hostname = identity, access stored).
	hosts, err := resolveHosts(st, []string{"ivan@dev1:tank", "ws"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 2 {
		t.Fatalf("got %+v", hosts)
	}

	// Bare run now uses the remembered inventory.
	hosts, err = resolveHosts(st, nil)
	if err != nil || len(hosts) != 2 {
		t.Fatalf("got %+v, %v", hosts, err)
	}
	if hosts[0].Name != "dev1" || hosts[0].User != "ivan" || hosts[0].Pool != "tank" {
		t.Errorf("dev1 access not remembered: %+v", hosts[0])
	}

	// A new spec for a known host replaces its access wholesale and limits
	// the active set.
	hosts, err = resolveHosts(st, []string{"dev1"})
	if err != nil || len(hosts) != 1 {
		t.Fatalf("got %+v, %v", hosts, err)
	}
	if hosts[0].User != "" || hosts[0].Pool != "" {
		t.Errorf("access not replaced: %+v", hosts[0])
	}
	all, _ := resolveHosts(st, nil)
	if len(all) != 2 { // ws still remembered
		t.Errorf("inventory: %+v", all)
	}
	for _, h := range all {
		if h.Name == "dev1" && (h.User != "" || h.Pool != "") {
			t.Errorf("stored access not replaced: %+v", h)
		}
	}
}
