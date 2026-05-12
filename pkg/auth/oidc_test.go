package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testKID = "test-key-1"

type oidcEnv struct {
	priv     *rsa.PrivateKey
	issuer   string // base URL of the httptest server (acts as the OIDC issuer)
	jwksURL  string
	teardown func()
}

func newOIDCEnv(t *testing.T) *oidcEnv {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa keygen: %v", err)
	}
	// JWKS handler — serves the matching public key with our test KID.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		jwks := buildJWKS(testKID, &priv.PublicKey)
		_ = json.NewEncoder(w).Encode(jwks)
	})
	srv := httptest.NewServer(mux)
	// Discovery doc — points jwks_uri at the JWKS endpoint above.
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		doc := map[string]any{
			"issuer":                 srv.URL,
			"jwks_uri":               srv.URL + "/jwks.json",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		}
		_ = json.NewEncoder(w).Encode(doc)
	})
	return &oidcEnv{
		priv:     priv,
		issuer:   srv.URL,
		jwksURL:  srv.URL + "/jwks.json",
		teardown: srv.Close,
	}
}

// buildJWKS returns a JSON-serializable JWKS containing one RSA
// public key under the given kid.
func buildJWKS(kid string, pub *rsa.PublicKey) map[string]any {
	n := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes())
	return map[string]any{
		"keys": []map[string]any{
			{
				"kty": "RSA",
				"use": "sig",
				"alg": "RS256",
				"kid": kid,
				"n":   n,
				"e":   e,
			},
		},
	}
}

func mintRS256(t *testing.T, env *oidcEnv, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = testKID
	s, err := tok.SignedString(env.priv)
	if err != nil {
		t.Fatalf("sign rs256: %v", err)
	}
	return s
}

func TestOIDCHappyPath_DirectJWKS(t *testing.T) {
	t.Parallel()
	env := newOIDCEnv(t)
	defer env.teardown()
	ctx := context.Background()
	a, err := NewOIDC(ctx, OIDCConfig{
		Issuer:   env.issuer,
		Audience: "clank-api",
		JWKSURL:  env.jwksURL,
	})
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	tok := mintRS256(t, env, jwt.MapClaims{
		"sub": "alice",
		"iss": env.issuer,
		"aud": "clank-api",
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
	})
	p, err := a.Verify(reqWithBearer(tok))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if p.UserID != "alice" {
		t.Errorf("UserID = %q, want alice", p.UserID)
	}
}

func TestOIDCDiscovery(t *testing.T) {
	t.Parallel()
	env := newOIDCEnv(t)
	defer env.teardown()
	ctx := context.Background()
	a, err := NewOIDC(ctx, OIDCConfig{
		Issuer:   env.issuer,
		Audience: "clank-api",
		// JWKSURL intentionally unset — should be discovered.
	})
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	tok := mintRS256(t, env, jwt.MapClaims{
		"sub": "alice",
		"iss": env.issuer,
		"aud": "clank-api",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	if _, err := a.Verify(reqWithBearer(tok)); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestOIDCRejectsWrongIssuer(t *testing.T) {
	t.Parallel()
	env := newOIDCEnv(t)
	defer env.teardown()
	a, err := NewOIDC(context.Background(), OIDCConfig{
		Issuer:   env.issuer,
		Audience: "clank-api",
		JWKSURL:  env.jwksURL,
	})
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	tok := mintRS256(t, env, jwt.MapClaims{
		"sub": "alice",
		"iss": "https://attacker.example.com",
		"aud": "clank-api",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	if _, err := a.Verify(reqWithBearer(tok)); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("err = %v, want ErrUnauthenticated", err)
	}
}

func TestOIDCRejectsWrongAudience(t *testing.T) {
	t.Parallel()
	env := newOIDCEnv(t)
	defer env.teardown()
	a, err := NewOIDC(context.Background(), OIDCConfig{
		Issuer:   env.issuer,
		Audience: "clank-api",
		JWKSURL:  env.jwksURL,
	})
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	tok := mintRS256(t, env, jwt.MapClaims{
		"sub": "alice",
		"iss": env.issuer,
		"aud": "other-service",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	if _, err := a.Verify(reqWithBearer(tok)); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("err = %v, want ErrUnauthenticated", err)
	}
}

func TestOIDCRejectsExpired(t *testing.T) {
	t.Parallel()
	env := newOIDCEnv(t)
	defer env.teardown()
	a, err := NewOIDC(context.Background(), OIDCConfig{
		Issuer:   env.issuer,
		Audience: "clank-api",
		JWKSURL:  env.jwksURL,
	})
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	tok := mintRS256(t, env, jwt.MapClaims{
		"sub": "alice",
		"iss": env.issuer,
		"aud": "clank-api",
		"exp": time.Now().Add(-time.Hour).Unix(),
	})
	if _, err := a.Verify(reqWithBearer(tok)); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("err = %v, want ErrUnauthenticated", err)
	}
}

func TestOIDCRejectsHS256(t *testing.T) {
	t.Parallel()
	env := newOIDCEnv(t)
	defer env.teardown()
	a, err := NewOIDC(context.Background(), OIDCConfig{
		Issuer:   env.issuer,
		Audience: "clank-api",
		JWKSURL:  env.jwksURL,
	})
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	// HS256 token — should be rejected because the default algorithms
	// list is ["RS256","ES256"].
	tok := mintHS256(t, []byte("anything"), jwt.MapClaims{
		"sub": "alice",
		"iss": env.issuer,
		"aud": "clank-api",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	if _, err := a.Verify(reqWithBearer(tok)); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("err = %v, want ErrUnauthenticated", err)
	}
}

func TestOIDCMissingExp(t *testing.T) {
	t.Parallel()
	env := newOIDCEnv(t)
	defer env.teardown()
	a, err := NewOIDC(context.Background(), OIDCConfig{
		Issuer:   env.issuer,
		Audience: "clank-api",
		JWKSURL:  env.jwksURL,
	})
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	tok := mintRS256(t, env, jwt.MapClaims{
		"sub": "alice",
		"iss": env.issuer,
		"aud": "clank-api",
		// no exp
	})
	if _, err := a.Verify(reqWithBearer(tok)); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("err = %v, want ErrUnauthenticated", err)
	}
}

func TestOIDCCustomUserClaim(t *testing.T) {
	t.Parallel()
	env := newOIDCEnv(t)
	defer env.teardown()
	a, err := NewOIDC(context.Background(), OIDCConfig{
		Issuer:    env.issuer,
		Audience:  "clank-api",
		JWKSURL:   env.jwksURL,
		UserClaim: "email",
	})
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	tok := mintRS256(t, env, jwt.MapClaims{
		"sub":   "internal-id-42",
		"email": "alice@example.com",
		"iss":   env.issuer,
		"aud":   "clank-api",
		"exp":   time.Now().Add(time.Hour).Unix(),
	})
	p, err := a.Verify(reqWithBearer(tok))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if p.UserID != "alice@example.com" {
		t.Errorf("UserID = %q, want alice@example.com", p.UserID)
	}
}

func TestOIDCDiscoveryFailures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "404",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			},
		},
		{
			name: "missing jwks_uri",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"issuer":"http://x"}`))
			},
		},
		{
			name: "malformed JSON",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`not-json`))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/.well-known/openid-configuration") {
					tc.handler(w, r)
					return
				}
				w.WriteHeader(http.StatusNotFound)
			}))
			defer srv.Close()
			_, err := NewOIDC(context.Background(), OIDCConfig{
				Issuer:   srv.URL,
				Audience: "clank-api",
			})
			if err == nil {
				t.Fatal("expected discovery error")
			}
		})
	}
}

func TestOIDCInvalidConfig(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  OIDCConfig
	}{
		{name: "missing issuer", cfg: OIDCConfig{Audience: "x", JWKSURL: "http://x"}},
		{name: "missing audience", cfg: OIDCConfig{Issuer: "http://x", JWKSURL: "http://x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewOIDC(context.Background(), tc.cfg)
			if err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

