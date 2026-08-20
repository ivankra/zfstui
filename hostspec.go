// Host spec parsing and inventory resolution.
//
// A host spec on the command line is one of:
//
//	host                                      (user defaults to root)
//	user@host[:pool]                          (scp-like; path = zfs pool)
//	ssh://[user[;opt=val...]@]host[:port][/pool]
//
// Options in the ssh:// userinfo (chrome-style) carry extra per-host params:
// send_user=NAME, recv_user=NAME. (sudo is implied: non-root user => sudo -n)
//
// The hostname is the identity everywhere (db key, table column); the rest
// is access specification, remembered in sqlite and replaced wholesale when
// a new spec is given for the same host.
package main

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// parseHostSpec parses one command-line host spec.
func parseHostSpec(spec string) (Host, error) {
	var h Host
	if strings.Contains(spec, "://") {
		if !strings.HasPrefix(spec, "ssh://") {
			return h, fmt.Errorf("host spec %q: only ssh:// URLs are supported", spec)
		}
		u, err := url.Parse(spec)
		if err != nil {
			return h, fmt.Errorf("host spec %q: %w", spec, err)
		}
		h.Name = u.Hostname()
		if p := u.Port(); p != "" {
			h.Port, err = strconv.Atoi(p)
			if err != nil {
				return h, fmt.Errorf("host spec %q: bad port: %w", spec, err)
			}
		}
		h.Pool = strings.Trim(u.Path, "/")
		if u.User != nil {
			userinfo := u.User.String() // may contain ;opt=val parts
			parts := strings.Split(userinfo, ";")
			h.User = parts[0]
			for _, opt := range parts[1:] {
				k, v, ok := strings.Cut(opt, "=")
				if !ok {
					return h, fmt.Errorf("host spec %q: option %q is not key=value", spec, opt)
				}
				switch k {
				case "send_user":
					h.SendUser = v
				case "recv_user":
					h.RecvUser = v
				default:
					return h, fmt.Errorf("host spec %q: unknown option %q (known: send_user, recv_user)", spec, k)
				}
			}
		}
	} else {
		// scp-like: [user@]host[:pool]
		rest := spec
		if user, hostPart, ok := strings.Cut(rest, "@"); ok {
			h.User = user
			rest = hostPart
		}
		if name, pool, ok := strings.Cut(rest, ":"); ok {
			h.Name = name
			h.Pool = pool
		} else {
			h.Name = rest
		}
	}
	if h.Name == "" {
		return h, fmt.Errorf("host spec %q: empty hostname", spec)
	}
	if strings.ContainsAny(h.Name, "@:/ ") {
		return h, fmt.Errorf("host spec %q: invalid hostname %q", spec, h.Name)
	}
	if strings.HasPrefix(h.Name, "-") {
		// A leading dash is never a legal hostname and is almost always a
		// misplaced flag (`status host -a` leaks `-a` here); reject it so it
		// can't be remembered as a host or handed to ssh as an option.
		return h, fmt.Errorf("host spec %q: hostname cannot start with %q (misplaced flag?)", spec, "-")
	}
	if h.User == "root" {
		h.User = "" // root is the default; normalize for SameAccess/storage
	}
	return h, nil
}

// parseHostSpecs parses all specs, rejecting conflicting duplicates.
func parseHostSpecs(specs []string) ([]Host, error) {
	var hosts []Host
	byName := map[string]Host{}
	for _, s := range specs {
		h, err := parseHostSpec(s)
		if err != nil {
			return nil, err
		}
		if prev, ok := byName[h.Name]; ok {
			if !prev.SameAccess(h) {
				return nil, fmt.Errorf("conflicting access specs for host %q: %s vs %s",
					h.Name, prev.URL(), h.URL())
			}
			continue // identical duplicate
		}
		byName[h.Name] = h
		hosts = append(hosts, h)
	}
	return hosts, nil
}

// resolveHosts builds the active host list. There is no config file: the
// remembered inventory IS sqlite, positional specs add/override and LIMIT
// the active set, and their access spec REPLACES whatever was stored.
// With no specs and nothing remembered, returns an error.
func resolveHosts(st *Store, specs []string) ([]Host, error) {
	if len(specs) > 0 {
		parsed, err := parseHostSpecs(specs)
		if err != nil {
			return nil, err
		}
		for _, h := range parsed {
			if err := st.UpsertHostAccess(h); err != nil {
				return nil, err
			}
		}
		return parsed, nil
	}
	stored, err := st.StoredHosts()
	if err != nil {
		return nil, err
	}
	if len(stored) == 0 {
		return nil, fmt.Errorf("no hosts known — pass host specs (e.g. `zfstui refresh root@host1 user@host2`)")
	}
	return stored, nil
}
