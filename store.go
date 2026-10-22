package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"sync"
)

type Store struct {
	db   *sql.DB
	mu   sync.Mutex
	aead cipher.AEAD
}

func openStore(dir string) (*Store, error) {
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	keyPath := filepath.Join(dir, "vault.key")
	key, e := os.ReadFile(keyPath)
	if os.IsNotExist(e) {
		key = make([]byte, 32)
		if _, e = rand.Read(key); e == nil {
			e = atomicWrite(keyPath, key, 0600)
		}
	}
	if e != nil {
		return nil, e
	}
	block, e := aes.NewCipher(key)
	if e != nil {
		return nil, e
	}
	aead, e := cipher.NewGCM(block)
	if e != nil {
		return nil, e
	}
	db, e := sql.Open("sqlite", filepath.Join(dir, "controller.db"))
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	_, e = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; CREATE TABLE IF NOT EXISTS records(kind TEXT NOT NULL,id TEXT NOT NULL,value BLOB NOT NULL,PRIMARY KEY(kind,id));`)
	if e != nil {
		db.Close()
		return nil, e
	}
	os.Chmod(filepath.Join(dir, "controller.db"), 0600)
	return &Store{db: db, aead: aead}, nil
}
func (s *Store) put(kind, id string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	_, e = s.db.Exec("INSERT INTO records(kind,id,value) VALUES(?,?,?) ON CONFLICT(kind,id) DO UPDATE SET value=excluded.value", kind, id, b)
	return e
}
func (s *Store) get(kind, id string, v any) error {
	var b []byte
	e := s.db.QueryRow("SELECT value FROM records WHERE kind=? AND id=?", kind, id).Scan(&b)
	if e != nil {
		return e
	}
	return json.Unmarshal(b, v)
}
func (s *Store) list(kind string, v any) error {
	rows, e := s.db.Query("SELECT value FROM records WHERE kind=? ORDER BY id", kind)
	if e != nil {
		return e
	}
	defer rows.Close()
	arr := []json.RawMessage{}
	for rows.Next() {
		var b []byte
		if e = rows.Scan(&b); e != nil {
			return e
		}
		arr = append(arr, b)
	}
	if e = rows.Err(); e != nil {
		return e
	}
	b, _ := json.Marshal(arr)
	return json.Unmarshal(b, v)
}
func (s *Store) del(kind, id string) error {
	_, e := s.db.Exec("DELETE FROM records WHERE kind=? AND id=?", kind, id)
	return e
}
func (s *Store) secret(id, value string) error {
	nonce := make([]byte, s.aead.NonceSize())
	if _, e := rand.Read(nonce); e != nil {
		return e
	}
	sealed := s.aead.Seal(nonce, nonce, []byte(value), []byte(id))
	return s.put("secrets", id, base64.StdEncoding.EncodeToString(sealed))
}
func (s *Store) unsecret(id string) (string, error) {
	var text string
	if e := s.get("secrets", id, &text); e != nil {
		return "", e
	}
	b, e := base64.StdEncoding.DecodeString(text)
	n := s.aead.NonceSize()
	if e != nil || len(b) < n {
		return "", errors.New("invalid secret")
	}
	v, e := s.aead.Open(nil, b[:n], b[n:], []byte(id))
	return string(v), e
}
