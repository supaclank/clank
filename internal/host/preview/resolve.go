package preview

import (
	"errors"
	"fmt"

	"github.com/acksell/clank/internal/launchconfig"
	"github.com/acksell/clank/pkg/preview/tokens"
)

var (
	// ErrSetupRequired reports that a web project needs one-time launch setup.
	ErrSetupRequired = errors.New("preview: launch setup is required")
	// ErrInvalidLaunchConfig reports a present but unusable launch file.
	ErrInvalidLaunchConfig = errors.New("preview: launch configuration is invalid")
)

// SetupRequiredError carries the one-time connected-agent setup contract.
type SetupRequiredError struct {
	ProjectConfigPath string
	Prompt            string
}

func (e *SetupRequiredError) Error() string {
	return fmt.Sprintf("%s: create %s", ErrSetupRequired, e.ProjectConfigPath)
}

func (e *SetupRequiredError) Unwrap() error {
	return ErrSetupRequired
}

type resolvedLaunch struct {
	Spec        Spec
	WorkDir     string
	ServiceName string
}

func resolveLaunch(workDir, name string) (*resolvedLaunch, error) {
	if name == "" {
		detected, err := Detect(workDir)
		if err != nil {
			return nil, err
		}
		if detected != nil && detected.Kind == KindExpo {
			return &resolvedLaunch{
				Spec:        *detected,
				WorkDir:     workDir,
				ServiceName: tokens.DefaultServiceName,
			}, nil
		}
	}

	configured, err := launchconfig.Resolve(workDir, name)
	if err != nil {
		var missing *launchconfig.NotFoundError
		if errors.As(err, &missing) {
			prompt, promptErr := launchconfig.SetupTaskPrompt(missing.Paths)
			if promptErr != nil {
				return nil, fmt.Errorf("%w: build setup task: %v", ErrInvalidLaunchConfig, promptErr)
			}
			return nil, &SetupRequiredError{
				ProjectConfigPath: missing.Paths.Project,
				Prompt:            prompt,
			}
		}
		return nil, fmt.Errorf("%w: %v", ErrInvalidLaunchConfig, err)
	}
	return &resolvedLaunch{
		Spec: Spec{
			Kind:        KindWeb,
			CmdTemplate: []string{"sh", "-c", configured.Command},
			ReadyProbe: ReadyProbe{
				Path:           configured.Ready.Path,
				ExpectedSubstr: configured.Ready.ExpectedSubstring,
			},
		},
		WorkDir:     configured.WorkDir,
		ServiceName: configured.Name,
	}, nil
}
