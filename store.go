// Package store is the sqlite cache of discovered inventory. It is a pure
// cache: fully rebuildable from the hosts, no authoritative data lives here.
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

// Bump whenever the schema changes: the db is a disposable cache, so on
// mismatch we just drop everything and start over (no migrations).
const schemaVersion = 5

const schema = `
CREATE TABLE IF NOT EXISTS hosts (
  name TEXT PRIMARY KEY,                    -- hostname: the identity
  -- access spec (latest one given; replaced wholesale by new CLI specs)
  user TEXT NOT NULL DEFAULT '',            -- '' = root
  port INTEGER NOT NULL DEFAULT 0,
  pool TEXT NOT NULL DEFAULT '',
  send_user TEXT NOT NULL DEFAULT '',
  recv_user TEXT NOT NULL DEFAULT '',
  -- refresh metadata
  last_refresh INTEGER NOT NULL DEFAULT 0,  -- unix seconds; 0 = never
  reachable INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT ''
);
-- Inventory objects are stored as their full property maps (JSON, from
-- "zpool/zfs get -Hp all"); typed fields are re-derived on load via the
-- same fromProps mapping the parser uses.
CREATE TABLE IF NOT EXISTS pools (
  host TEXT NOT NULL, name TEXT NOT NULL,
  props TEXT NOT NULL DEFAULT '{}', srcs TEXT NOT NULL DEFAULT '{}',
  PRIMARY KEY (host, name)
);
CREATE TABLE IF NOT EXISTS datasets (
  host TEXT NOT NULL, name TEXT NOT NULL,
  props TEXT NOT NULL DEFAULT '{}', srcs TEXT NOT NULL DEFAULT '{}',
  PRIMARY KEY (host, name)
);
CREATE TABLE IF NOT EXISTS snapshots (
  host TEXT NOT NULL, dataset TEXT NOT NULL, name TEXT NOT NULL,
  props TEXT NOT NULL DEFAULT '{}', srcs TEXT NOT NULL DEFAULT '{}',
  PRIMARY KEY (host, dataset, name)
);
CREATE TABLE IF NOT EXISTS vdevs (
  host TEXT NOT NULL, pool TEXT NOT NULL, name TEXT NOT NULL,
  ord INTEGER NOT NULL DEFAULT 0,           -- preserve tree order
  props TEXT NOT NULL DEFAULT '{}', srcs TEXT NOT NULL DEFAULT '{}',
  PRIMARY KEY (host, pool, name)
);
CREATE TABLE IF NOT EXISTS disks (
  host TEXT NOT NULL, id TEXT NOT NULL,
  props TEXT NOT NULL DEFAULT '{}',
  PRIMARY KEY (host, id)
);
CREATE TABLE IF NOT EXISTS jobs (
  host TEXT NOT NULL, id TEXT NOT NULL, dir TEXT NOT NULL,
  kind TEXT, state TEXT, status TEXT, meta TEXT,
  progress INTEGER, expected INTEGER,
  PRIMARY KEY (host, id)
);
`

// Open opens (creating if needed) the cache at path; ":memory:" works.
func openStore(path string) (*Store, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// One connection: serializes concurrent refresh writers (no SQLITE_BUSY)
	// and keeps ":memory:" meaning one database rather than one per pool conn.
	db.SetMaxOpenConns(1)
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		db.Close()
		return nil, err
	}
	if version != schemaVersion {
		// Stale or fresh db: it's a disposable cache, recreate from scratch.
		for _, t := range []string{"hosts", "pools", "datasets", "snapshots", "vdevs", "smart", "jobs", "hidden_overrides"} {
			if _, err := db.Exec("DROP TABLE IF EXISTS " + t); err != nil {
				db.Close()
				return nil, err
			}
		}
		if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
			db.Close()
			return nil, err
		}
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// UpsertHostAccess remembers (or replaces) a host's access spec, leaving
// refresh metadata alone.
func (s *Store) UpsertHostAccess(h Host) error {
	_, err := s.db.Exec(
		`INSERT INTO hosts (name, user, port, pool, send_user, recv_user) VALUES (?,?,?,?,?,?)
		 ON CONFLICT(name) DO UPDATE SET user=excluded.user, port=excluded.port,
		   pool=excluded.pool, send_user=excluded.send_user, recv_user=excluded.recv_user`,
		h.Name, h.User, h.Port, h.Pool, h.SendUser, h.RecvUser)
	return err
}

// StoredHosts returns the remembered inventory with access specs.
func (s *Store) StoredHosts() ([]Host, error) {
	rows, err := s.db.Query(`SELECT name, user, port, pool, send_user, recv_user FROM hosts ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Host
	for rows.Next() {
		var h Host
		if err := rows.Scan(&h.Name, &h.User, &h.Port, &h.Pool, &h.SendUser, &h.RecvUser); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// PutHostState replaces all cached inventory for one host atomically.
func (s *Store) PutHostState(st HostState) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range []string{"pools", "datasets", "snapshots", "vdevs", "disks", "jobs"} {
		if _, err := tx.Exec("DELETE FROM "+table+" WHERE host = ?", st.Host); err != nil {
			return err
		}
	}
	mj := func(m map[string]string) string {
		if m == nil {
			m = map[string]string{}
		}
		b, _ := json.Marshal(m)
		return string(b)
	}
	for _, p := range st.Pools {
		if _, err := tx.Exec(
			`INSERT INTO pools (host,name,props,srcs) VALUES (?,?,?,?)`,
			p.Host, p.Name, mj(p.toProps()), mj(p.PropSrc)); err != nil {
			return err
		}
	}
	for _, d := range st.Datasets {
		if _, err := tx.Exec(
			`INSERT INTO datasets (host,name,props,srcs) VALUES (?,?,?,?)`,
			d.Host, d.Name, mj(d.toProps()), mj(d.PropSrc)); err != nil {
			return err
		}
	}
	for _, sn := range st.Snapshots {
		if _, err := tx.Exec(
			`INSERT INTO snapshots (host,dataset,name,props,srcs) VALUES (?,?,?,?,?)`,
			sn.Host, sn.Dataset, sn.Name, mj(sn.toProps()), mj(sn.PropSrc)); err != nil {
			return err
		}
	}
	for i, v := range st.VDevs {
		if _, err := tx.Exec(
			`INSERT INTO vdevs (host,pool,name,ord,props,srcs) VALUES (?,?,?,?,?,?)`,
			v.Host, v.Pool, v.Name, i, mj(v.toProps()), mj(v.PropSrc)); err != nil {
			return err
		}
	}
	for _, d := range st.Disks {
		if _, err := tx.Exec(
			`INSERT INTO disks (host,id,props) VALUES (?,?,?)`,
			d.Host, d.ID, mj(d.toProps())); err != nil {
			return err
		}
	}
	for _, j := range st.Jobs {
		meta, _ := json.Marshal(j.Meta)
		if _, err := tx.Exec(
			`INSERT INTO jobs (host,id,dir,kind,state,status,meta,progress,expected) VALUES (?,?,?,?,?,?,?,?,?)`,
			j.Host, j.ID, j.Dir, j.Kind, j.State, j.Status, string(meta), j.Progress, j.Expected); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO hosts (name, last_refresh, reachable, last_error) VALUES (?,?,1,'')
		 ON CONFLICT(name) DO UPDATE SET last_refresh=excluded.last_refresh, reachable=1, last_error=''`,
		st.Host, time.Now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkUnreachable records a failed refresh; cached inventory is kept.
func (s *Store) MarkUnreachable(host string, reason error) error {
	_, err := s.db.Exec(
		`INSERT INTO hosts (name, reachable, last_error) VALUES (?,0,?)
		 ON CONFLICT(name) DO UPDATE SET reachable=0, last_error=excluded.last_error`,
		host, reason.Error())
	return err
}

// HostInfo is refresh metadata for one host.
type HostInfo struct {
	Name        string
	LastRefresh time.Time
	Reachable   bool
	LastError   string
}

func (s *Store) Hosts() ([]HostInfo, error) {
	rows, err := s.db.Query(`SELECT name, last_refresh, reachable, last_error FROM hosts ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HostInfo
	for rows.Next() {
		var h HostInfo
		var ts int64
		var reach int
		if err := rows.Scan(&h.Name, &ts, &reach, &h.LastError); err != nil {
			return nil, err
		}
		h.LastRefresh = time.Unix(ts, 0)
		h.Reachable = reach != 0
		out = append(out, h)
	}
	return out, rows.Err()
}

// AllHostStates loads the cached inventory for every host.
func (s *Store) AllHostStates() ([]HostState, error) {
	byHost := map[string]*HostState{}
	order := []string{}
	get := func(host string) *HostState {
		if st, ok := byHost[host]; ok {
			return st
		}
		st := &HostState{Host: host}
		byHost[host] = st
		order = append(order, host)
		return st
	}

	unm := func(s string) map[string]string {
		m := map[string]string{}
		_ = json.Unmarshal([]byte(s), &m)
		return m
	}
	rows, err := s.db.Query(`SELECT host,name,props,srcs FROM pools ORDER BY host,name`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var host, name, props, srcs string
		if err := rows.Scan(&host, &name, &props, &srcs); err != nil {
			rows.Close()
			return nil, err
		}
		st := get(host)
		st.Pools = append(st.Pools, poolFromProps(host, name, unm(props), unm(srcs)))
	}
	rows.Close()

	rows, err = s.db.Query(`SELECT host,name,props,srcs FROM datasets ORDER BY host,name`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var host, name, props, srcs string
		if err := rows.Scan(&host, &name, &props, &srcs); err != nil {
			rows.Close()
			return nil, err
		}
		st := get(host)
		st.Datasets = append(st.Datasets, datasetFromProps(host, name, unm(props), unm(srcs)))
	}
	rows.Close()

	rows, err = s.db.Query(`SELECT host,pool,name,props,srcs FROM vdevs ORDER BY host,pool,ord`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var host, pool, name, props, srcs string
		if err := rows.Scan(&host, &pool, &name, &props, &srcs); err != nil {
			rows.Close()
			return nil, err
		}
		st := get(host)
		st.VDevs = append(st.VDevs, vdevFromProps(host, pool, name, unm(props), unm(srcs)))
	}
	rows.Close()

	rows, err = s.db.Query(`SELECT host,id,props FROM disks ORDER BY host,id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var host, id, props string
		if err := rows.Scan(&host, &id, &props); err != nil {
			rows.Close()
			return nil, err
		}
		st := get(host)
		d := diskFromProps(host, unm(props))
		if d.ID == "" {
			d.ID = id
		}
		if isPseudoDiskName(firstNonEmpty(d.KName, baseName(d.Dev))) {
			continue
		}
		st.Disks = append(st.Disks, d)
	}
	rows.Close()

	rows, err = s.db.Query(`SELECT host,id,dir,kind,state,status,meta,progress,expected FROM jobs ORDER BY host,id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var j JobInfo
		var meta string
		if err := rows.Scan(&j.Host, &j.ID, &j.Dir, &j.Kind, &j.State, &j.Status, &meta, &j.Progress, &j.Expected); err != nil {
			rows.Close()
			return nil, err
		}
		j.Meta = map[string]string{}
		_ = json.Unmarshal([]byte(meta), &j.Meta)
		st := get(j.Host)
		st.Jobs = append(st.Jobs, j)
	}
	rows.Close()

	rows, err = s.db.Query(`SELECT host,dataset,name,props,srcs FROM snapshots ORDER BY host,dataset,name`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var host, ds, name, props, srcs string
		if err := rows.Scan(&host, &ds, &name, &props, &srcs); err != nil {
			rows.Close()
			return nil, err
		}
		st := get(host)
		st.Snapshots = append(st.Snapshots, snapshotFromProps(host, ds, name, unm(props), unm(srcs)))
	}
	rows.Close()

	// Snapshot order matters downstream (latest = last): creation order is
	// in the props now, not a sortable column.
	for _, st := range byHost {
		sort.SliceStable(st.Snapshots, func(a, b int) bool {
			x, y := st.Snapshots[a], st.Snapshots[b]
			if x.Dataset != y.Dataset {
				return x.Dataset < y.Dataset
			}
			if x.Creation != y.Creation {
				return x.Creation < y.Creation
			}
			return x.CreateTXG < y.CreateTXG
		})
		attachDiskIDsToVDevs(st.VDevs, diskAliasesFromDisks(st.Disks))
	}

	var out []HostState
	for _, h := range order {
		out = append(out, *byHost[h])
	}
	return out, nil
}

// Dev-mode default (cwd-relative for easy inspection; XDG before release).
func defaultDBPath() string { return "zfstui.db" }
