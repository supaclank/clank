package blobstore

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxLANUpload caps a single PUT to keep a misbehaving (or hostile) LAN
// peer from filling the disk. Generous for phone photos; images are the
// only thing on this path.
const maxLANUpload = 25 << 20 // 25 MiB

// LAN is a self-serving, disk-backed Storage for single-binary
// deployments (clank preview) where there is no S3. It runs its own HTTP
// server on a LAN-bound listener and mints HMAC-signed presigned URLs that
// point back at itself, so a phone can PUT an image and the local
// clank-host can GET it — both over the LAN, no object store required.
//
// Presigned URLs are signed over (key, op, exp) with a per-process key, so
// a peer on the same network can't PUT or GET by guessing storage keys:
// only URLs this server minted are honored, and only until they expire.
type LAN struct {
	dir     string
	baseURL string
	signKey []byte

	srv *http.Server
	ln  net.Listener
	wg  sync.WaitGroup
}

// NewLAN starts the blob server on bindAddr (e.g. "0.0.0.0:0") and
// advertises advertiseHost (the LAN IP) in every minted URL so the URLs
// resolve from both the phone and the local host. It owns a private temp
// directory for blobs. signKey must be non-empty. Caller MUST Close.
func NewLAN(bindAddr, advertiseHost string, signKey []byte) (*LAN, error) {
	if len(signKey) == 0 {
		return nil, errors.New("blobstore.NewLAN: signKey is required")
	}
	if advertiseHost == "" {
		return nil, errors.New("blobstore.NewLAN: advertiseHost is required")
	}
	dir, err := os.MkdirTemp("", "clank-preview-blobs-")
	if err != nil {
		return nil, fmt.Errorf("blobstore.NewLAN: temp dir: %w", err)
	}
	ln, err := net.Listen("tcp", bindAddr)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("blobstore.NewLAN: listen %s: %w", bindAddr, err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	l := &LAN{
		dir:     dir,
		baseURL: fmt.Sprintf("http://%s:%d", advertiseHost, port),
		signKey: signKey,
		ln:      ln,
	}
	l.srv = &http.Server{Handler: http.HandlerFunc(l.handle)}
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		_ = l.srv.Serve(ln)
	}()
	return l, nil
}

// BaseURL is the advertised origin minted URLs hang off of, e.g.
// http://192.168.1.20:7879.
func (l *LAN) BaseURL() string { return l.baseURL }

// Close stops the server and removes the blob directory.
func (l *LAN) Close() error {
	err := l.srv.Close()
	l.wg.Wait()
	_ = os.RemoveAll(l.dir)
	return err
}

func (l *LAN) PresignPut(_ context.Context, key string, ttl time.Duration) (string, error) {
	return l.signedURL(key, "put", ttl)
}

func (l *LAN) PresignGet(_ context.Context, key string, ttl time.Duration) (string, error) {
	return l.signedURL(key, "get", ttl)
}

func (l *LAN) Exists(_ context.Context, key string) (bool, error) {
	p, err := l.pathFor(key)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(p)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// DeletePrefix removes every blob whose key starts with prefix. Walks the
// tree (the store is small + ephemeral) so partial prefixes work, not just
// directory boundaries.
func (l *LAN) DeletePrefix(_ context.Context, prefix string) error {
	if prefix == "" {
		return fmt.Errorf("DeletePrefix: empty prefix would sweep the entire store")
	}
	return filepath.WalkDir(l.dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, relErr := filepath.Rel(l.dir, p)
		if relErr != nil {
			return relErr
		}
		if strings.HasPrefix(filepath.ToSlash(rel), prefix) {
			return os.Remove(p)
		}
		return nil
	})
}

func (l *LAN) signedURL(key, op string, ttl time.Duration) (string, error) {
	if key == "" {
		return "", errors.New("blobstore.LAN: empty key")
	}
	exp := time.Now().Add(ttl).Unix()
	q := url.Values{}
	q.Set("op", op)
	q.Set("exp", strconv.FormatInt(exp, 10))
	q.Set("sig", l.sign(key, op, exp))
	u := url.URL{Path: "/" + key, RawQuery: q.Encode()}
	return l.baseURL + u.String(), nil
}

func (l *LAN) sign(key, op string, exp int64) string {
	mac := hmac.New(sha256.New, l.signKey)
	fmt.Fprintf(mac, "%s\n%s\n%d", key, op, exp)
	return hex.EncodeToString(mac.Sum(nil))
}

// pathFor maps a storage key to a contained on-disk path, rejecting any
// key that would escape the blob directory.
func (l *LAN) pathFor(key string) (string, error) {
	clean := filepath.Join(l.dir, filepath.FromSlash(key))
	if clean != l.dir && !strings.HasPrefix(clean, l.dir+string(os.PathSeparator)) {
		return "", fmt.Errorf("blobstore.LAN: key %q escapes store", key)
	}
	return clean, nil
}

func (l *LAN) handle(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/")
	op := r.URL.Query().Get("op")
	expRaw := r.URL.Query().Get("exp")
	sig := r.URL.Query().Get("sig")

	exp, err := strconv.ParseInt(expRaw, 10, 64)
	if err != nil {
		http.Error(w, "bad exp", http.StatusBadRequest)
		return
	}
	if time.Now().Unix() > exp {
		http.Error(w, "url expired", http.StatusForbidden)
		return
	}
	if subtle.ConstantTimeCompare([]byte(sig), []byte(l.sign(key, op, exp))) != 1 {
		http.Error(w, "bad signature", http.StatusForbidden)
		return
	}

	p, err := l.pathFor(key)
	if err != nil {
		http.Error(w, "bad key", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodPut:
		if op != "put" {
			http.Error(w, "op mismatch", http.StatusForbidden)
			return
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			http.Error(w, "mkdir", http.StatusInternalServerError)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxLANUpload+1))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(body) > maxLANUpload {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		if err := os.WriteFile(p, body, 0o600); err != nil {
			http.Error(w, "write", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)

	case http.MethodGet, http.MethodHead:
		if op != "get" {
			http.Error(w, "op mismatch", http.StatusForbidden)
			return
		}
		f, err := os.Open(p)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		defer f.Close()
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = io.Copy(w, f)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// compile-time check
var _ Storage = (*LAN)(nil)
