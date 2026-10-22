package main

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (m *Managed) backups() (any, error) {
	dir := filepath.Join(m.root, "backups")
	os.MkdirAll(dir, 0700)
	es, e := os.ReadDir(dir)
	if e != nil {
		return nil, e
	}
	out := []map[string]any{}
	for _, f := range es {
		if !strings.HasSuffix(f.Name(), ".zip") {
			continue
		}
		st, e := f.Info()
		if e == nil {
			out = append(out, map[string]any{"name": f.Name(), "size": st.Size(), "created": st.ModTime()})
		}
	}
	return out, nil
}
func (m *Managed) backup() (any, error) {
	if !m.op.TryLock() {
		return nil, errors.New("server is busy")
	}
	if m.snapshot().PID != 0 {
		m.op.Unlock()
		return nil, errors.New("stop the server first so the backup contains a consistent saved world")
	}
	name := time.Now().UTC().Format("2006-01-02_15-04-05") + "-" + token(3) + ".zip"
	m.setState("backing_up", "Creating backup")
	go func() {
		defer m.op.Unlock()
		e := m.makeBackup(name)
		if e != nil {
			m.setState("error", "Backup failed: "+e.Error())
			return
		}
		m.setState("stopped", "Backup saved: "+name)
	}()
	return map[string]string{"message": "Backup started", "name": name}, nil
}
func (m *Managed) makeBackup(name string) error {
	dir := filepath.Join(m.root, "backups")
	os.MkdirAll(dir, 0700)
	path := filepath.Join(dir, name)
	temp := path + ".partial"
	defer os.Remove(temp)
	f, e := os.OpenFile(temp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	z := zip.NewWriter(f)
	root := filepath.Join(m.root, "game")
	e = filepath.WalkDir(root, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("cannot back up symlink %s", p)
		}
		if d.IsDir() {
			return nil
		}
		rel, e := filepath.Rel(root, p)
		if e != nil {
			return e
		}
		st, e := d.Info()
		if e != nil {
			return e
		}
		h, e := zip.FileInfoHeader(st)
		if e != nil {
			return e
		}
		h.Name = filepath.ToSlash(rel)
		h.Method = zip.Deflate
		w, e := z.CreateHeader(h)
		if e != nil {
			return e
		}
		r, e := os.Open(p)
		if e != nil {
			return e
		}
		_, e = io.Copy(w, r)
		r.Close()
		return e
	})
	ze := z.Close()
	fe := f.Close()
	if e != nil {
		return e
	}
	if ze != nil {
		return ze
	}
	if fe != nil {
		return fe
	}
	return os.Rename(temp, path)
}
func (m *Managed) restore(name string) error {
	if filepath.Base(name) != name || !strings.HasSuffix(name, ".zip") {
		return errors.New("invalid backup name")
	}
	if !m.op.TryLock() {
		return errors.New("server is busy")
	}
	if m.snapshot().PID != 0 {
		m.op.Unlock()
		return errors.New("stop the server before restoring")
	}
	archive := filepath.Join(m.root, "backups", name)
	if _, e := os.Stat(archive); e != nil {
		m.op.Unlock()
		return e
	}
	m.setState("restoring", "Restoring backup; current files will be kept")
	go func() {
		defer m.op.Unlock()
		stage, e := os.MkdirTemp(m.root, "restore-")
		if e != nil {
			m.setState("error", e.Error())
			return
		}
		defer os.RemoveAll(stage)
		if e = extract(archive, stage); e != nil {
			m.setState("error", e.Error())
			return
		}
		game := filepath.Join(m.root, "game")
		old := filepath.Join(m.root, "before-restore-"+time.Now().UTC().Format("20060102-150405")+"-"+token(3))
		if e = os.Rename(game, old); e != nil {
			m.setState("error", e.Error())
			return
		}
		if e = os.Rename(stage, game); e != nil {
			os.Rename(old, game)
			m.setState("error", e.Error())
			return
		}
		m.setState("stopped", "Backup restored. Previous files preserved in "+filepath.Base(old))
	}()
	return nil
}
