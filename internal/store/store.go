// Package store is the sqlite cache of discovered inventory. It is a pure
// cache: fully rebuildable from the hosts, no authoritative data lives here.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"ivank.me/zfstui/internal/model"
)

type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS hosts (
  name TEXT PRIMARY KEY,
  last_refresh INTEGER NOT NULL DEFAULT 0,  -- unix seconds; 0 = never
  reachable INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS pools (
  host TEXT NOT NULL, name TEXT NOT NULL, guid TEXT NOT NULL DEFAULT '',
  size INTEGER, alloc INTEGER, free INTEGER, health TEXT,
  PRIMARY KEY (host, name)
);
CREATE TABLE IF NOT EXISTS datasets (
  host TEXT NOT NULL, name TEXT NOT NULL, type TEXT NOT NULL,
  guid TEXT NOT NULL DEFAULT '',
  used INTEGER, avail INTEGER, refer INTEGER,
  creation INTEGER, createtxg INTEGER, written INTEGER,
  origin TEXT, encryption TEXT, keystatus TEXT, keylocation TEXT,
  mounted INTEGER, canmount TEXT, mountpoint TEXT, readonly INTEGER,
  resume_token TEXT, compression TEXT, recordsize INTEGER,
  PRIMARY KEY (host, name)
);
CREATE TABLE IF NOT EXISTS snapshots (
  host TEXT NOT NULL, dataset TEXT NOT NULL, name TEXT NOT NULL,
  guid TEXT NOT NULL DEFAULT '',
  used INTEGER, creation INTEGER, createtxg INTEGER,
  PRIMARY KEY (host, dataset, name)
);
CREATE INDEX IF NOT EXISTS snapshots_guid ON snapshots (guid);
`

// Open opens (creating if needed) the cache at path; ":memory:" works.
func Open(path string) (*Store, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// PutHostState replaces all cached inventory for one host atomically.
func (s *Store) PutHostState(st model.HostState) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range []string{"pools", "datasets", "snapshots"} {
		if _, err := tx.Exec("DELETE FROM "+table+" WHERE host = ?", st.Host); err != nil {
			return err
		}
	}
	for _, p := range st.Pools {
		if _, err := tx.Exec(
			`INSERT INTO pools (host,name,guid,size,alloc,free,health) VALUES (?,?,?,?,?,?,?)`,
			p.Host, p.Name, p.GUID, p.Size, p.Alloc, p.Free, p.Health); err != nil {
			return err
		}
	}
	for _, d := range st.Datasets {
		if _, err := tx.Exec(
			`INSERT INTO datasets (host,name,type,guid,used,avail,refer,creation,createtxg,written,
			   origin,encryption,keystatus,keylocation,mounted,canmount,mountpoint,readonly,
			   resume_token,compression,recordsize)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			d.Host, d.Name, d.Type, d.GUID, d.Used, d.Avail, d.Refer, d.Creation, d.CreateTXG,
			d.Written, d.Origin, d.Encryption, d.KeyStatus, d.KeyLocation, d.Mounted, d.CanMount,
			d.Mountpoint, d.ReadOnly, d.ResumeToken, d.Compression, d.RecordSize); err != nil {
			return err
		}
	}
	for _, sn := range st.Snapshots {
		if _, err := tx.Exec(
			`INSERT INTO snapshots (host,dataset,name,guid,used,creation,createtxg) VALUES (?,?,?,?,?,?,?)`,
			sn.Host, sn.Dataset, sn.Name, sn.GUID, sn.Used, sn.Creation, sn.CreateTXG); err != nil {
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
func (s *Store) AllHostStates() ([]model.HostState, error) {
	byHost := map[string]*model.HostState{}
	order := []string{}
	get := func(host string) *model.HostState {
		if st, ok := byHost[host]; ok {
			return st
		}
		st := &model.HostState{Host: host}
		byHost[host] = st
		order = append(order, host)
		return st
	}

	rows, err := s.db.Query(`SELECT host,name,guid,size,alloc,free,health FROM pools ORDER BY host,name`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p model.Pool
		if err := rows.Scan(&p.Host, &p.Name, &p.GUID, &p.Size, &p.Alloc, &p.Free, &p.Health); err != nil {
			rows.Close()
			return nil, err
		}
		st := get(p.Host)
		st.Pools = append(st.Pools, p)
	}
	rows.Close()

	rows, err = s.db.Query(`SELECT host,name,type,guid,used,avail,refer,creation,createtxg,written,
		origin,encryption,keystatus,keylocation,mounted,canmount,mountpoint,readonly,
		resume_token,compression,recordsize FROM datasets ORDER BY host,name`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var d model.Dataset
		if err := rows.Scan(&d.Host, &d.Name, &d.Type, &d.GUID, &d.Used, &d.Avail, &d.Refer,
			&d.Creation, &d.CreateTXG, &d.Written, &d.Origin, &d.Encryption, &d.KeyStatus,
			&d.KeyLocation, &d.Mounted, &d.CanMount, &d.Mountpoint, &d.ReadOnly,
			&d.ResumeToken, &d.Compression, &d.RecordSize); err != nil {
			rows.Close()
			return nil, err
		}
		st := get(d.Host)
		st.Datasets = append(st.Datasets, d)
	}
	rows.Close()

	rows, err = s.db.Query(`SELECT host,dataset,name,guid,used,creation,createtxg FROM snapshots ORDER BY host,dataset,creation`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var sn model.Snapshot
		if err := rows.Scan(&sn.Host, &sn.Dataset, &sn.Name, &sn.GUID, &sn.Used, &sn.Creation, &sn.CreateTXG); err != nil {
			rows.Close()
			return nil, err
		}
		st := get(sn.Host)
		st.Snapshots = append(st.Snapshots, sn)
	}
	rows.Close()

	var out []model.HostState
	for _, h := range order {
		out = append(out, *byHost[h])
	}
	return out, nil
}
