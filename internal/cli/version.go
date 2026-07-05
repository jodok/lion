package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// version is overridable at build time via -ldflags "-X ...".
var version = "dev"

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the lion version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "lion "+version)
			return err
		},
	}
}
