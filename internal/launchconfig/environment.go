package launchconfig

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var managedEnvironmentNames = map[string]struct{}{
	PortEnvironmentName:           {},
	PublicHostnameEnvironmentName: {},
	PublicURLEnvironmentName:      {},
}

var supportedEnvironmentPlaceholders = map[string]struct{}{
	PortEnvironmentName:           {},
	PublicHostnameEnvironmentName: {},
}

// RenderEnvironment resolves Clank placeholders in configured environment values.
func RenderEnvironment(environment map[string]string, port int, publicHostname string) (map[string]string, error) {
	if port <= 0 {
		return nil, fmt.Errorf("render environment: port must be positive")
	}
	if strings.TrimSpace(publicHostname) == "" {
		return nil, fmt.Errorf("render environment: public hostname is required")
	}
	if err := validateEnvironment(environment); err != nil {
		return nil, err
	}

	variables := map[string]string{
		PortEnvironmentName:           strconv.Itoa(port),
		PublicHostnameEnvironmentName: publicHostname,
	}
	rendered := make(map[string]string, len(environment))
	for name, value := range environment {
		expanded, err := renderEnvironmentValue(value, variables)
		if err != nil {
			return nil, fmt.Errorf("environment variable %q: %w", name, err)
		}
		rendered[name] = expanded
	}
	return rendered, nil
}

func validateEnvironment(environment map[string]string) error {
	names := make([]string, 0, len(environment))
	for name := range environment {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !environmentNamePattern.MatchString(name) {
			return fmt.Errorf("invalid environment variable name %q", name)
		}
		if _, managed := managedEnvironmentNames[name]; managed {
			return fmt.Errorf("environment variable %q is managed by Clank", name)
		}
		if _, err := renderEnvironmentValue(environment[name], map[string]string{
			PortEnvironmentName:           "",
			PublicHostnameEnvironmentName: "",
		}); err != nil {
			return fmt.Errorf("environment variable %q: %w", name, err)
		}
	}
	return nil
}

func renderEnvironmentValue(value string, variables map[string]string) (string, error) {
	var rendered strings.Builder
	for {
		start := strings.Index(value, "${")
		if start < 0 {
			rendered.WriteString(value)
			return rendered.String(), nil
		}
		rendered.WriteString(value[:start])
		value = value[start+2:]
		end := strings.IndexByte(value, '}')
		if end < 0 {
			return "", fmt.Errorf("unterminated environment placeholder")
		}
		name := value[:end]
		if _, supported := supportedEnvironmentPlaceholders[name]; !supported {
			return "", fmt.Errorf("unsupported placeholder ${%s}", name)
		}
		rendered.WriteString(variables[name])
		value = value[end+1:]
	}
}

func cloneEnvironment(environment map[string]string) map[string]string {
	if environment == nil {
		return nil
	}
	cloned := make(map[string]string, len(environment))
	for name, value := range environment {
		cloned[name] = value
	}
	return cloned
}
