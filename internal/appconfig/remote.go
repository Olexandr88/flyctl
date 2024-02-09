package appconfig

import (
	"context"
	"fmt"

	"github.com/superfly/flyctl/api"
	"github.com/superfly/flyctl/client"
	"github.com/superfly/flyctl/flaps"
	"github.com/superfly/flyctl/gql"
	"github.com/superfly/flyctl/internal/machine"
	"github.com/superfly/flyctl/iostreams"
)

func FromRemoteApp(ctx context.Context, appName string) (*Config, error) {
	apiClient := client.FromContext(ctx).API()

	appCompact, err := apiClient.GetAppCompact(ctx, appName)
	if err != nil {
		return nil, fmt.Errorf("error getting app: %w", err)
	}

	return getConfig(ctx, apiClient, appCompact)
}

func FromAppCompact(ctx context.Context, appCompact *api.AppCompact) (*Config, error) {
	apiClient := client.FromContext(ctx).API()

	return getConfig(ctx, apiClient, appCompact)
}

func getConfig(ctx context.Context, apiClient *api.Client, appCompact *api.AppCompact) (*Config, error) {
	appName := appCompact.Name

	cfg, err := getAppV2ConfigFromReleases(ctx, apiClient, appName)
	if cfg == nil {
		cfg, err = getAppV2ConfigFromMachines(ctx, apiClient, appCompact)
	}
	if err != nil {
		return nil, err
	}
	if err := cfg.SetMachinesPlatform(); err != nil {
		return nil, err
	}
	cfg.AppName = appName
	return cfg, nil
}

func getAppV2ConfigFromMachines(ctx context.Context, apiClient *api.Client, appCompact *api.AppCompact) (*Config, error) {
	flapsClient := flaps.FromContext(ctx)
	io := iostreams.FromContext(ctx)
	activeMachines, err := machine.ListActive(ctx)
	if err != nil {
		return nil, fmt.Errorf("error listing active machines for %s app: %w", appCompact.Name, err)
	}
	machineSet := machine.NewMachineSet(flapsClient, io, activeMachines)
	appConfig, warnings, err := FromAppAndMachineSet(ctx, appCompact, machineSet)
	if err != nil {
		return nil, fmt.Errorf("failed to grab app config from existing machines, error: %w", err)
	}
	if warnings != "" {
		fmt.Fprintf(io.ErrOut, "WARNINGS:\n%s", warnings)
	}
	return appConfig, nil
}

func getAppV2ConfigFromReleases(ctx context.Context, apiClient *api.Client, appName string) (*Config, error) {
	_ = `# @genqlient
	query FlyctlConfigCurrentRelease($appName: String!) {
		app(name:$appName) {
			currentReleaseUnprocessed {
				configDefinition
			}
		}
	}
	`
	resp, err := gql.FlyctlConfigCurrentRelease(ctx, apiClient.GenqClient, appName)
	if err != nil {
		return nil, err
	}

	configDefinition := resp.App.CurrentReleaseUnprocessed.ConfigDefinition
	if configDefinition == nil {
		return nil, nil
	}

	configMapDefinition, ok := configDefinition.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("likely a bug, could not convert config definition of type %T to api map[string]any", configDefinition)
	}

	appConfig, err := FromDefinition(api.DefinitionPtr(configMapDefinition))
	if err != nil {
		return nil, fmt.Errorf("error creating appv2 Config from api definition: %w", err)
	}
	return appConfig, err
}
