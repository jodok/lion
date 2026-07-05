package cli

import (
	"encoding/json"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// schema is a machine-readable description of lion's command tree, flags, and
// exit codes — the contract agents and scripts introspect instead of scraping
// --help. Mirrors gogcli's `schema --json`.
func init() { registerCommand(newSchemaCmd) }

type schemaFlag struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Usage   string `json:"usage"`
	Default string `json:"default,omitempty"`
}

type schemaCommand struct {
	Path        string          `json:"path"`
	Short       string          `json:"short"`
	Flags       []schemaFlag    `json:"flags,omitempty"`
	Subcommands []schemaCommand `json:"subcommands,omitempty"`
}

type schemaDoc struct {
	Name        string          `json:"name"`
	Version     string          `json:"version"`
	GlobalFlags []schemaFlag    `json:"global_flags"`
	Commands    []schemaCommand `json:"commands"`
	ExitCodes   map[string]int  `json:"exit_codes"`
}

func newSchemaCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "schema",
		Short: "Emit a machine-readable description of the CLI (JSON)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			root := cmd.Root()
			doc := schemaDoc{
				Name:        root.Name(),
				Version:     version,
				GlobalFlags: collectFlags(root, true),
				ExitCodes: map[string]int{
					"ok":           ExitOK,
					"error":        ExitError,
					"usage":        ExitUsage,
					"auth":         ExitAuth,
					"rate_limited": ExitRateLimited,
					"not_found":    ExitNotFound,
					"permission":   ExitPermission,
					"challenge":    ExitChallenge,
				},
			}
			for _, c := range root.Commands() {
				if c.Hidden || c.Name() == "help" || c.Name() == "completion" {
					continue
				}
				doc.Commands = append(doc.Commands, describe(c))
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(doc)
		},
	}
}

// describe recursively builds the schema for a command and its subcommands.
func describe(c *cobra.Command) schemaCommand {
	sc := schemaCommand{
		Path:  c.CommandPath(),
		Short: c.Short,
		Flags: collectFlags(c, false),
	}
	for _, child := range c.Commands() {
		if child.Hidden {
			continue
		}
		sc.Subcommands = append(sc.Subcommands, describe(child))
	}
	return sc
}

// collectFlags returns a command's flags. When global is true it reads
// persistent (inherited) flags; otherwise only the command's local flags.
func collectFlags(c *cobra.Command, global bool) []schemaFlag {
	var out []schemaFlag
	set := c.LocalFlags()
	if global {
		set = c.PersistentFlags()
	}
	set.VisitAll(func(f *pflag.Flag) {
		out = append(out, schemaFlag{
			Name:    f.Name,
			Type:    f.Value.Type(),
			Usage:   f.Usage,
			Default: f.DefValue,
		})
	})
	return out
}
