package daemoncli

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/acksell/clank/pkg/auth"
)

// Auth mode selector env vars. Exactly one must be set in TCP mode
// (and not at all in Unix-socket mode); resolveDefaultAuth enforces.
const (
	envJWTSecret     = "CLANK_AUTH_JWT_SECRET"     // HS256 mode
	envStaticToken   = "CLANK_AUTH_TOKEN"          // Static-bearer mode
	envAllowStatic   = "CLANK_AUTH_ALLOW_STATIC"   // Opt-in for static-bearer
	envOIDCIssuer    = "CLANK_AUTH_OIDC_ISSUER"    // OIDC mode
	envOIDCAudience  = "CLANK_AUTH_OIDC_AUDIENCE"  // required when OIDC mode on
	envOIDCJWKSURL   = "CLANK_AUTH_OIDC_JWKS_URL"  // optional; defaults via discovery
	envOIDCUserClaim = "CLANK_AUTH_OIDC_USER_CLAIM"
	envOIDCAlgs      = "CLANK_AUTH_OIDC_ALGORITHMS" // comma-separated
)

// resolveDefaultAuth picks the right Authenticator for the listener
// mode and returns a short description for the startup log line.
// Caller should use opts.Auth instead when non-nil — this is only
// invoked for the env-driven default path.
func resolveDefaultAuth(ctx context.Context, opts ServerOptions) (auth.Authenticator, string, error) {
	if opts.Listen == "" {
		// Unix-socket: file permissions are the gate; every request
		// resolves to the OS user.
		return &auth.AllowAll{UserID: staticUserID()}, "auth.AllowAll (unix socket)", nil
	}

	jwt := strings.TrimSpace(os.Getenv(envJWTSecret))
	tok := strings.TrimSpace(os.Getenv(envStaticToken))
	issuer := strings.TrimSpace(os.Getenv(envOIDCIssuer))

	set := 0
	for _, v := range []string{jwt, tok, issuer} {
		if v != "" {
			set++
		}
	}
	switch {
	case set == 0:
		return nil, "", fmt.Errorf(
			"--listen tcp:// requires one of %s, %s (with %s=true), or %s",
			envJWTSecret, envStaticToken, envAllowStatic, envOIDCIssuer)
	case set > 1:
		return nil, "", fmt.Errorf(
			"set exactly one of %s, %s, %s — got %d set",
			envJWTSecret, envStaticToken, envOIDCIssuer, set)
	}

	switch {
	case issuer != "":
		audience := strings.TrimSpace(os.Getenv(envOIDCAudience))
		if audience == "" {
			return nil, "", fmt.Errorf("%s requires %s", envOIDCIssuer, envOIDCAudience)
		}
		cfg := auth.OIDCConfig{
			Issuer:    issuer,
			Audience:  audience,
			JWKSURL:   strings.TrimSpace(os.Getenv(envOIDCJWKSURL)),
			UserClaim: strings.TrimSpace(os.Getenv(envOIDCUserClaim)),
		}
		if raw := strings.TrimSpace(os.Getenv(envOIDCAlgs)); raw != "" {
			for _, a := range strings.Split(raw, ",") {
				if a = strings.TrimSpace(a); a != "" {
					cfg.Algorithms = append(cfg.Algorithms, a)
				}
			}
		}
		// Cap the initial JWKS fetch at 10s so a misconfigured issuer
		// surfaces fast instead of hanging startup.
		fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		oidc, err := auth.NewOIDC(fetchCtx, cfg)
		if err != nil {
			return nil, "", err
		}
		return oidc, fmt.Sprintf("auth.OIDC (issuer=%s, aud=%s)", issuer, audience), nil

	case jwt != "":
		return &auth.JWTHS256{Secret: []byte(jwt)}, "auth.JWTHS256 (HS256, env secret)", nil

	case tok != "":
		if !boolEnv(envAllowStatic) {
			return nil, "", errors.New(envStaticToken + " is set but " + envAllowStatic +
				"=true is not — static-bearer auth must be opted into explicitly")
		}
		return &auth.StaticBearer{Token: tok, UserID: staticUserID()},
			"auth.StaticBearer (static bearer, single-user)", nil
	}

	return nil, "", errors.New("unreachable: resolveDefaultAuth fell through")
}

// staticUserID returns the OS username for the running process, or
// "local" if it can't be determined. Used for unix-socket and
// static-bearer modes where the JWT sub claim isn't available.
func staticUserID() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "local"
}

// boolEnv returns true when the env var is set to a truthy value
// (1, true, yes, on — case-insensitive). Anything else is false.
func boolEnv(key string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// logAuthMode emits a one-line startup banner naming the active
// Authenticator. Surfaces misconfiguration immediately — operators
// running the wrong mode notice on the first log line.
func logAuthMode(description string) {
	log.Printf("gateway: %s", description)
}
