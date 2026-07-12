package flymachines

import (
	"errors"
	"fmt"
	"net/url"
	"testing"

	fly "github.com/superfly/fly-go"
	"github.com/superfly/fly-go/flaps"
)

// TestIsNotFound_OnlyTrustsTypedError pins that a transport error whose
// text happens to contain "404" (an app-name hash does, ~1/315) is NOT
// read as a 404 — misreading it triggers destructive recovery.
func TestIsNotFound_OnlyTrustsTypedError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"typed 404", &flaps.FlapsError{ResponseStatusCode: 404}, true},
		{"typed 500", &flaps.FlapsError{ResponseStatusCode: 500}, false},
		{"typed 404 wrapped", fmt.Errorf("get app: %w", &flaps.FlapsError{ResponseStatusCode: 404}), true},
		{
			"transport error with 404 in the app name",
			&url.Error{Op: "Get", URL: "https://api.machines.dev/v1/apps/clank-u-a404bc12de34/machines", Err: errors.New("dial tcp: timeout")},
			false,
		},
		{"literal 'not found' text but untyped", errors.New("resource not found"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := isNotFound(c.err); got != c.want {
				t.Errorf("isNotFound(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestWithDefaults_RejectsVolumeAboveExtendLimit(t *testing.T) {
	t.Parallel()
	base := Options{APIToken: "t", OrgSlug: "o", Region: "arn", Image: "img"}

	ok := base
	ok.VolumeSizeGB = DefaultVolumeSizeLimitGB
	if _, err := ok.withDefaults(); err != nil {
		t.Errorf("VolumeSizeGB == limit should be accepted, got %v", err)
	}

	bad := base
	bad.VolumeSizeGB = DefaultVolumeSizeLimitGB + 1
	if _, err := bad.withDefaults(); err == nil {
		t.Error("VolumeSizeGB > limit should fail fast, got nil")
	}
}

func TestBuildMachineConfig_TemplatesJSON(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	opts.TemplatesJSON = `[{"display_name":"Expo","clone_url":"https://x/y.git","source":"builtin"}]`
	cfg := buildMachineConfig(opts, testTokens(), "vol_1", nil)
	if cfg.Env["CLANK_TEMPLATES"] != opts.TemplatesJSON {
		t.Fatalf("CLANK_TEMPLATES = %q, want the catalog JSON", cfg.Env["CLANK_TEMPLATES"])
	}

	// Steady-state: it must survive the drift reconcile (else machine-
	// backed hosts serve an empty catalog after the first update).
	want := buildMachineConfig(opts, testTokens(), "vol_1", oneShotEnv(cfg))
	if needsUpdate(cfg, want) {
		t.Error("templates env read as drift — would restart every EnsureHost / get stripped")
	}

	empty := testOptions(t)
	if _, ok := buildMachineConfig(empty, testTokens(), "vol_1", nil).Env["CLANK_TEMPLATES"]; ok {
		t.Error("CLANK_TEMPLATES set when TemplatesJSON is empty")
	}
}

func TestBuildMachineConfig_SizeLimitTracksVolumeSize(t *testing.T) {
	t.Parallel()
	// withDefaults caps VolumeSizeGB at the limit, but buildMachineConfig
	// must never emit a SizeGbLimit below the volume's own size.
	opts := testOptions(t)
	opts.VolumeSizeGB = DefaultVolumeSizeLimitGB // the max allowed
	cfg := buildMachineConfig(opts, testTokens(), "vol_1", nil)
	if got := cfg.Mounts[0].SizeGbLimit; got < opts.VolumeSizeGB {
		t.Fatalf("SizeGbLimit %d < VolumeSizeGB %d", got, opts.VolumeSizeGB)
	}
}

func TestIsAlreadyStopped(t *testing.T) {
	t.Parallel()
	cases := []struct {
		msg  string
		want bool
	}{
		{"machine already stopped", true},
		{"machine is already in stopped state", true},
		{"lease is already held by another instance", false},
		{"machine is in destroying state", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isAlreadyStopped(errors.New(c.msg)); got != c.want {
			t.Errorf("isAlreadyStopped(%q) = %v, want %v", c.msg, got, c.want)
		}
	}
	if isAlreadyStopped(nil) {
		t.Error("isAlreadyStopped(nil) = true")
	}
}

func TestHostPortOf(t *testing.T) {
	t.Parallel()
	got, err := hostPortOf("http://[fdaa:0:1::2]:8080")
	if err != nil || got != "[fdaa:0:1::2]:8080" {
		t.Fatalf("valid URL: got %q err %v", got, err)
	}
	// A garbage flycast IP must fail fast, not silently return the raw
	// string (which would break bearer pinning and, upstream, panic in
	// client.Do(nil) before this fix).
	if _, err := hostPortOf("http://[]:8080"); err == nil {
		t.Error("malformed URL should error")
	}
	if _, err := hostPortOf("::not a url"); err == nil {
		t.Error("unparseable URL should error")
	}
}

// compile-time: the review-fix build config still satisfies the guest
// preset contract.
var _ = fly.MachineConfig{}
