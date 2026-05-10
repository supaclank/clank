package storage

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Memory is an in-memory Storage implementation backed by a httptest
// server, intended for tests. Real PUT/GET against the presigned URLs
// works (the embedded server handles them) so the same client code
// path can be exercised end-to-end without a real S3.
//
// Not safe for production use: presigned URLs do not carry signatures
// (only TTL), there is no access control, and all data lives in
// process memory.
type Memory struct {
	srv *httptest.Server

	mu      sync.Mutex
	objects map[string][]byte
}

// NewMemory constructs a Memory backend with an embedded test server.
// Callers MUST invoke Close() to release the server when done.
func NewMemory() *Memory {
	m := &Memory{
		objects: make(map[string][]byte),
	}
	m.srv = httptest.NewServer(http.HandlerFunc(m.handle))
	return m
}

// Close shuts down the embedded test server.
func (m *Memory) Close() {
	m.srv.Close()
}

// BaseURL returns the embedded test server's base URL — useful for
// tests that need to hand the URL to a separate component.
func (m *Memory) BaseURL() string { return m.srv.URL }

// Put writes data at key directly, bypassing the presigned-URL path.
// Useful in tests for arrange-phase setup.
func (m *Memory) Put(key string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	m.objects[key] = cp
}

// Get reads data at key directly, bypassing the presigned-URL path.
func (m *Memory) Get(key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.objects[key]
	if !ok {
		return nil, false
	}
	cp := make([]byte, len(d))
	copy(cp, d)
	return cp, true
}

// Keys returns all keys currently stored. Useful for assertions.
func (m *Memory) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.objects))
	for k := range m.objects {
		out = append(out, k)
	}
	return out
}

func (m *Memory) PresignPut(_ context.Context, key string, ttl time.Duration) (string, error) {
	return m.signedURL(key, "put", ttl), nil
}

func (m *Memory) PresignGet(_ context.Context, key string, ttl time.Duration) (string, error) {
	return m.signedURL(key, "get", ttl), nil
}

func (m *Memory) Exists(_ context.Context, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.objects[key]
	return ok, nil
}

func (m *Memory) signedURL(key, op string, ttl time.Duration) string {
	exp := time.Now().Add(ttl).Unix()
	q := url.Values{}
	q.Set("op", op)
	q.Set("exp", strconv.FormatInt(exp, 10))
	// TODO(coderabbit): escape key via url.URL.Path/RawQuery to mirror real S3 behavior
	// https://github.com/Acksell/clank/pull/16#discussion_r3213462004
	return m.srv.URL + "/" + key + "?" + q.Encode()
}

func (m *Memory) handle(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/")
	op := r.URL.Query().Get("op")
	expRaw := r.URL.Query().Get("exp")

	exp, err := strconv.ParseInt(expRaw, 10, 64)
	if err != nil {
		http.Error(w, "bad exp", http.StatusBadRequest)
		return
	}
	if time.Now().Unix() > exp {
		http.Error(w, "url expired", http.StatusForbidden)
		return
	}

	switch r.Method {
	case http.MethodPut:
		if op != "put" {
			http.Error(w, fmt.Sprintf("op mismatch: url is %q, method is PUT", op), http.StatusForbidden)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		m.objects[key] = body
		m.mu.Unlock()
		w.WriteHeader(http.StatusOK)

	case http.MethodGet:
		if op != "get" {
			http.Error(w, fmt.Sprintf("op mismatch: url is %q, method is GET", op), http.StatusForbidden)
			return
		}
		m.mu.Lock()
		data, ok := m.objects[key]
		m.mu.Unlock()
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_, _ = w.Write(data)

	case http.MethodHead:
		if op != "get" {
			http.Error(w, "op mismatch", http.StatusForbidden)
			return
		}
		m.mu.Lock()
		_, ok := m.objects[key]
		m.mu.Unlock()
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// compile-time check
var _ Storage = (*Memory)(nil)
