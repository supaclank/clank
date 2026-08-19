package preview

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/supaclank/clank/internal/launchconfig"
)

const (
	localPreviewHostname      = "127.0.0.1"
	expoNoDotenvName          = "EXPO_NO_DOTENV"
	expoProxyURLName          = "EXPO_PACKAGER_PROXY_URL"
	previewRuntimeName        = "CLANK_PREVIEW_RUNTIME"
	nodeOptionsName           = "NODE_OPTIONS"
	continuousIntegrationName = "CI"
)

type environmentRequest struct {
	Kind            Kind
	MarkerPath      string
	PublicURL       string
	ShimRequirePath string
	RuntimePath     string
	Port            int
	Configured      map[string]string
}

type previewEndpoint struct {
	PublicHostname string
	PublicURL      string
	ReadinessHost  string
}

func prepareEnvironment(req environmentRequest) ([]string, string, error) {
	endpoint, err := resolvePreviewEndpoint(req.Kind, req.PublicURL, req.Port)
	if err != nil {
		return nil, "", err
	}
	env, err := buildEnvironment(req, endpoint)
	if err != nil {
		return nil, "", err
	}
	return env, endpoint.ReadinessHost, nil
}

func buildEnv(req environmentRequest) ([]string, error) {
	env, _, err := prepareEnvironment(req)
	return env, err
}

func resolvePreviewEndpoint(kind Kind, publicURL string, port int) (previewEndpoint, error) {
	if kind != KindWeb {
		return previewEndpoint{}, nil
	}
	if publicURL == "" {
		loopbackHost := net.JoinHostPort(localPreviewHostname, strconv.Itoa(port))
		return previewEndpoint{
			PublicHostname: localPreviewHostname,
			ReadinessHost:  loopbackHost,
		}, nil
	}
	parsed, err := url.Parse(publicURL)
	if err != nil {
		return previewEndpoint{}, fmt.Errorf("parse preview public URL: %w", err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return previewEndpoint{}, fmt.Errorf("preview public URL %q must be an absolute HTTP URL", publicURL)
	}
	// TODO(ai-review): ReadinessHost keeps PublicURL's port while PublicHostname
	// strips it; an exact-match (non-port-stripping) host validator would reject
	// readiness when PublicURL carries an explicit port. https://github.com/supaclank/clank/pull/214#discussion_r3697014299
	return previewEndpoint{
		PublicHostname: parsed.Hostname(),
		PublicURL:      publicURL,
		ReadinessHost:  parsed.Host,
	}, nil
}

// buildEnvironment merges configured web values over the inherited process
// environment while keeping Clank's runtime variables authoritative.
func buildEnvironment(req environmentRequest, endpoint previewEndpoint) ([]string, error) {
	if req.Port <= 0 {
		return nil, fmt.Errorf("build preview environment: port must be positive")
	}
	if req.Kind == KindExpo && len(req.Configured) != 0 {
		return nil, fmt.Errorf("build preview environment: configured environment is only supported for web previews")
	}

	configured := map[string]string(nil)
	if req.Kind == KindWeb {
		var err error
		configured, err = launchconfig.RenderEnvironment(req.Configured, req.Port, endpoint.PublicHostname)
		if err != nil {
			return nil, fmt.Errorf("build preview environment: %w", err)
		}
	}

	parent := os.Environ()
	env := make([]string, 0, len(parent)+len(configured)+6)
	requireFlag := ""
	if req.ShimRequirePath != "" {
		requireFlag = "--require " + req.ShimRequirePath
	}

	nodeOptionsMerged := false
	for _, entry := range parent {
		key, _, _ := strings.Cut(entry, "=")
		if _, overridden := configured[key]; overridden {
			continue
		}
		if key == launchconfig.PortEnvironmentName || key == launchconfig.PublicHostnameEnvironmentName || key == launchconfig.PublicURLEnvironmentName {
			continue
		}
		if req.Kind == KindExpo {
			switch key {
			case continuousIntegrationName, expoNoDotenvName, bootstrapMarkerEnv, previewRuntimeName, expoProxyURLName:
				continue
			}
		}
		if requireFlag != "" && key == nodeOptionsName {
			env = append(env, entry+" "+requireFlag)
			nodeOptionsMerged = true
			continue
		}
		env = append(env, entry)
	}

	env = append(env, launchconfig.PortEnvironmentName+"="+strconv.Itoa(req.Port))
	if req.Kind == KindWeb {
		env = append(env, launchconfig.PublicHostnameEnvironmentName+"="+endpoint.PublicHostname)
		if endpoint.PublicURL != "" {
			env = append(env, launchconfig.PublicURLEnvironmentName+"="+endpoint.PublicURL)
		}
	}
	configuredNames := make([]string, 0, len(configured))
	for name := range configured {
		configuredNames = append(configuredNames, name)
	}
	sort.Strings(configuredNames)
	for _, name := range configuredNames {
		env = append(env, name+"="+configured[name])
	}

	if req.Kind == KindExpo {
		env = append(env, expoNoDotenvName+"=1", bootstrapMarkerEnv+"="+req.MarkerPath)
	}
	if requireFlag != "" && !nodeOptionsMerged {
		env = append(env, nodeOptionsName+"="+requireFlag)
	}
	if req.Kind == KindExpo && req.RuntimePath != "" {
		env = append(env, previewRuntimeName+"="+req.RuntimePath)
	}
	if req.Kind == KindExpo && req.PublicURL != "" {
		// Metro otherwise advertises its unreachable internal port to clients.
		env = append(env, expoProxyURLName+"="+req.PublicURL)
	}
	return env, nil
}
