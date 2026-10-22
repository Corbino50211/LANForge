package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 90 * time.Second}

func token(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func readJSON(path string, v any) error {
	b, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	return json.Unmarshal(b, v)
}
func atomicWrite(path string, b []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".lanforge-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(perm); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return replaceFile(tmp, path)
}
func writeJSON(path string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	return atomicWrite(path, b, 0600)
}
func inside(root, rel string) (string, error) {
	rel = strings.ReplaceAll(rel, "\\", "/")
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.Contains(rel, ":") {
		return "", errors.New("path must stay inside this server's folder")
	}
	p := filepath.Join(root, filepath.FromSlash(rel))
	r, e := filepath.Rel(root, p)
	if e != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes server folder")
	}
	// Reject symlinks at every existing component, including the root.
	cur := root
	if s, e := os.Lstat(cur); e == nil && s.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("symlink root is not allowed")
	}
	for _, part := range strings.Split(r, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		if s, e := os.Lstat(cur); e == nil && s.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("symlink paths are not allowed")
		}
	}
	return p, nil
}
func fetchJSON(raw string, out any) error {
	req, e := http.NewRequest("GET", raw, nil)
	if e != nil {
		return e
	}
	req.Header.Set("User-Agent", "LanForge/"+Version)
	res, e := httpClient.Do(req)
	if e != nil {
		return e
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("download returned HTTP %d from %s", res.StatusCode, req.URL.Host)
	}
	return json.NewDecoder(io.LimitReader(res.Body, 32<<20)).Decode(out)
}
func download(raw, dest, expected string, log func(string)) error {
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "https" {
		return errors.New("downloads must use HTTPS")
	}
	if e = os.MkdirAll(filepath.Dir(dest), 0700); e != nil {
		return e
	}
	req, e := http.NewRequest("GET", raw, nil)
	if e != nil {
		return e
	}
	req.Header.Set("User-Agent", "LanForge/"+Version)
	client := &http.Client{Timeout: 45 * time.Minute, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if r.URL.Scheme != "https" || len(via) > 10 {
			return errors.New("unsafe download redirect")
		}
		return nil
	}}
	res, e := client.Do(req)
	if e != nil {
		return e
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("download returned HTTP %d from %s", res.StatusCode, u.Host)
	}
	f, e := os.CreateTemp(filepath.Dir(dest), ".download-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	h := sha256.New()
	last := time.Now()
	var total int64
	buf := make([]byte, 128*1024)
	for {
		n, er := res.Body.Read(buf)
		if n > 0 {
			if _, e = f.Write(buf[:n]); e != nil {
				f.Close()
				return e
			}
			h.Write(buf[:n])
			total += int64(n)
			if total > 20<<30 {
				f.Close()
				return errors.New("download exceeds 20 GB safety limit")
			}
			if log != nil && time.Since(last) > 3*time.Second {
				log(fmt.Sprintf("Downloading: %.1f MB", float64(total)/(1<<20)))
				last = time.Now()
			}
		}
		if er == io.EOF {
			break
		}
		if er != nil {
			f.Close()
			return er
		}
	}
	if e = f.Close(); e != nil {
		return e
	}
	if expected != "" && !strings.EqualFold(strings.TrimPrefix(expected, "sha256:"), hex.EncodeToString(h.Sum(nil))) {
		return errors.New("download checksum mismatch")
	}
	return replaceFile(f.Name(), dest)
}
func extract(archive, dest string) error {
	var total int64
	write := func(name string, mode os.FileMode, size int64, r io.Reader) error {
		p, e := inside(dest, name)
		if e != nil {
			return e
		}
		if size < 0 || size > 8<<30 || total+size > 30<<30 {
			return errors.New("archive exceeds extraction limit")
		}
		total += size
		if e = os.MkdirAll(filepath.Dir(p), 0700); e != nil {
			return e
		}
		f, e := os.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode.Perm()&0777)
		if e != nil {
			return e
		}
		_, e = io.CopyN(f, r, size)
		ce := f.Close()
		if e != nil {
			return e
		}
		return ce
	}
	if strings.HasSuffix(strings.ToLower(archive), ".zip") {
		z, e := zip.OpenReader(archive)
		if e != nil {
			return e
		}
		defer z.Close()
		for _, f := range z.File {
			if f.Mode()&os.ModeSymlink != 0 {
				return errors.New("archive symlinks not permitted")
			}
			if f.FileInfo().IsDir() {
				if _, e := inside(dest, f.Name); e != nil {
					return e
				}
				continue
			}
			r, e := f.Open()
			if e != nil {
				return e
			}
			e = write(f.Name, f.Mode()|0600, int64(f.UncompressedSize64), r)
			r.Close()
			if e != nil {
				return e
			}
		}
		return nil
	}
	f, e := os.Open(archive)
	if e != nil {
		return e
	}
	defer f.Close()
	g, e := gzip.NewReader(f)
	if e != nil {
		return e
	}
	defer g.Close()
	t := tar.NewReader(g)
	type archiveLink struct{ name, target string }
	var links []archiveLink
	for {
		h, e := t.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if _, e := inside(dest, h.Name); e != nil {
				return e
			}
		case tar.TypeReg, tar.TypeRegA:
			if e = write(h.Name, os.FileMode(h.Mode)|0600, h.Size, t); e != nil {
				return e
			}
		case tar.TypeSymlink, tar.TypeLink:
			if _, e := inside(dest, h.Name); e != nil {
				return e
			}
			target := h.Linkname
			if filepath.IsAbs(target) || strings.Contains(target, ":") || strings.Contains(target, "\\") {
				return errors.New("unsafe archive link")
			}
			if h.Typeflag == tar.TypeSymlink {
				target = filepath.Join(filepath.Dir(h.Name), target)
			}
			if _, e := inside(dest, target); e != nil {
				return e
			}
			links = append(links, archiveLink{h.Name, target})
		default:
			return fmt.Errorf("archive entry %q is not a regular file", h.Name)
		}
	}
	// Materialize internal file links as copies. No filesystem symlink is created,
	// including on Windows where creating symlinks can require privileges.
	for len(links) > 0 {
		remaining := []archiveLink{}
		for _, link := range links {
			target, e := inside(dest, link.target)
			if e != nil {
				return e
			}
			info, e := os.Stat(target)
			if os.IsNotExist(e) {
				remaining = append(remaining, link)
				continue
			}
			if e != nil {
				return e
			}
			if !info.Mode().IsRegular() {
				return errors.New("archive link target must be a regular file")
			}
			output, e := inside(dest, link.name)
			if e != nil {
				return e
			}
			if output == target {
				return errors.New("self-referencing archive link")
			}
			f, e := os.Open(target)
			if e != nil {
				return e
			}
			e = write(link.name, info.Mode(), info.Size(), f)
			f.Close()
			if e != nil {
				return e
			}
		}
		if len(remaining) == len(links) {
			return errors.New("unresolved archive links")
		}
		links = remaining
	}
	return nil
}

var safeID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,100}$`)

func str(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
func asBool(v any) bool  { return strings.EqualFold(str(v), "true") }
func clone[T any](v T) T { b, _ := json.Marshal(v); var out T; json.Unmarshal(b, &out); return out }
