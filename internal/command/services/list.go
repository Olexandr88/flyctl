package services

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/superfly/flyctl/client"
	"github.com/superfly/flyctl/internal/appconfig"
	"github.com/superfly/flyctl/internal/command"
	"github.com/superfly/flyctl/internal/flag"
)

func newList() *cobra.Command {
	const (
		long  = "List the services that are associated with an app"
		short = "List services"
	)

	cmd := command.New("list", short, long, runList, command.RequireSession, command.RequireAppName)

	cmd.Aliases = []string{"ls"}

	flag.Add(cmd,
		flag.App(),
		flag.AppConfig(),
	)

	return cmd
}

func runList(ctx context.Context) error {
	var (
		client  = client.FromContext(ctx).API()
		appName = appconfig.NameFromContext(ctx)
	)

	appInfo, err := client.GetAppInfo(ctx, appName)
	if err != nil {
		return err
	}

	return ShowMachineServiceInfo(ctx, appInfo)
}
