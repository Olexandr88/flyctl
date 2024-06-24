package mysql

import (
	"github.com/spf13/cobra"
	"github.com/superfly/flyctl/internal/command"
)

func New() (cmd *cobra.Command) {

	const (
		short = "Provision and manage MySQL databases"
		long  = short + "\n"
	)

	cmd = command.New("mysql", short, long, nil)
	cmd.AddCommand(create(), destroy(), dashboard(), list(), status())

	return cmd
}
