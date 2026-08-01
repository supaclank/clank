package clankcli

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	previewHTTP              = "http"
	previewHTTPS             = "https"
	previewIPv4LoopbackHost  = "127.0.0.1"
	previewLocalhostHostname = "localhost"
)

type previewAttachArgs struct {
	ProjectDir  string
	UpstreamURL *url.URL
}

// parsePreviewAttachArgs recognizes the explicit attach form. The folder is
// mandatory because it defines the project context exposed to the overlay and
// agent; the target only defines where browser traffic is proxied.
func parsePreviewAttachArgs(args []string) (*previewAttachArgs, error) {
	if len(args) == 0 {
		return nil, nil
	}
	if len(args) == 1 {
		_, isTarget, err := parsePreviewTarget(args[0])
		if err != nil {
			return nil, err
		}
		if isTarget {
			return nil, fmt.Errorf("a preview folder is required with a URL; use clank preview . %s", args[0])
		}
		return nil, nil
	}
	if len(args) != 2 {
		return nil, fmt.Errorf("preview attach accepts exactly one folder and one URL or :port")
	}

	var target *url.URL
	var folder string
	for _, arg := range args {
		parsed, isTarget, err := parsePreviewTarget(arg)
		if err != nil {
			return nil, err
		}
		if isTarget {
			if target != nil {
				return nil, fmt.Errorf("preview attach requires exactly one folder and one URL or :port")
			}
			target = parsed
			continue
		}
		folder = arg
	}
	if target == nil {
		return nil, fmt.Errorf("preview attach requires one URL or :port alongside the folder")
	}

	projectDir, err := resolveExplicitPreviewDir(folder)
	if err != nil {
		return nil, err
	}
	return &previewAttachArgs{ProjectDir: projectDir, UpstreamURL: target}, nil
}

func parsePreviewTarget(raw string) (*url.URL, bool, error) {
	if strings.HasPrefix(raw, ":") {
		port, err := strconv.Atoi(strings.TrimPrefix(raw, ":"))
		if err != nil || port < 1 || port > 65535 {
			return nil, true, fmt.Errorf("invalid preview port shorthand %q: port must be between 1 and 65535", raw)
		}
		return &url.URL{
			Scheme: previewHTTP,
			Host:   net.JoinHostPort(previewIPv4LoopbackHost, strconv.Itoa(port)),
		}, true, nil
	}
	if !strings.Contains(raw, "://") {
		return nil, false, nil
	}

	target, err := url.Parse(raw)
	if err != nil {
		return nil, true, fmt.Errorf("parse preview URL %q: %w", raw, err)
	}
	target.Scheme = strings.ToLower(target.Scheme)
	if target.Scheme != previewHTTP && target.Scheme != previewHTTPS {
		return nil, true, fmt.Errorf("preview URL must use http or https, got %q", target.Scheme)
	}
	if target.Host == "" {
		return nil, true, fmt.Errorf("preview URL %q must be absolute", raw)
	}
	if target.User != nil || target.RawQuery != "" || target.ForceQuery || target.Fragment != "" || (target.Path != "" && target.Path != "/") {
		return nil, true, fmt.Errorf("preview URL must be an origin only (for example http://127.0.0.1:5173), got %q", raw)
	}
	target.Path = ""
	target.RawPath = ""
	if !isPreviewLoopbackHost(target.Hostname()) {
		return nil, true, fmt.Errorf("preview URL must use a loopback host such as 127.0.0.1 or localhost, got %q", target.Hostname())
	}
	return target, true, nil
}

func isPreviewLoopbackHost(hostname string) bool {
	if strings.EqualFold(hostname, previewLocalhostHostname) {
		return true
	}
	ip := net.ParseIP(hostname)
	return ip != nil && ip.IsLoopback()
}

func resolveExplicitPreviewDir(folder string) (string, error) {
	abs, err := filepath.Abs(folder)
	if err != nil {
		return "", fmt.Errorf("resolve preview folder %q: %w", folder, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("preview folder %q: %w", folder, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("preview folder %q is not a directory", folder)
	}
	return abs, nil
}

func previewLoopbackURL(port int) *url.URL {
	return &url.URL{
		Scheme: previewHTTP,
		Host:   net.JoinHostPort(previewIPv4LoopbackHost, strconv.Itoa(port)),
	}
}
