package cli

import (
	"context"
	"fmt"

	"github.com/jodok/lion/internal/output"
	"github.com/spf13/cobra"
)

// Verticals self-register so adding one never requires editing root.go.
func init() { registerCommand(newConnectionCmd) }

func newConnectionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "connection",
		Short: "Manage LinkedIn connections and invitations",
	}
	cmd.AddCommand(
		newConnectionListCmd(),
		newConnectionInviteCmd(),
		newConnectionAcceptCmd(),
		newConnectionRemoveCmd(),
		newConnectionRequestsCmd(),
	)
	return cmd
}

func newConnectionListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List your first-degree connections",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app := appFrom(cmd)
			cl, err := app.Client()
			if err != nil {
				return err
			}
			conns, err := cl.Connections(context.Background(), app.Cfg.Max)
			if err != nil {
				return err
			}
			r := app.Renderer()
			if app.Cfg.JSON {
				return r.Emit(conns)
			}
			t := &output.Table{Cols: []string{"PUBLIC_ID", "NAME", "HEADLINE"}}
			for _, c := range conns {
				t.Rows = append(t.Rows, []string{c.PublicID, c.Name, r.Untrusted(c.Headline)})
			}
			return r.Emit(t)
		},
	}
}

func newConnectionInviteCmd() *cobra.Command {
	var note string
	cmd := &cobra.Command{
		Use:   "invite <id>",
		Short: "Send a connection invitation",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app := appFrom(cmd)
			if err := app.requireWritable(); err != nil {
				return err
			}
			cl, err := app.Client()
			if err != nil {
				return err
			}
			if err := cl.Invite(context.Background(), args[0], note); err != nil {
				return err
			}
			r := app.Renderer()
			if app.Cfg.JSON {
				return r.Emit(map[string]string{"public_id": args[0], "status": "invited"})
			}
			t := &output.Table{Cols: []string{"PUBLIC_ID", "STATUS"}}
			t.Rows = [][]string{{args[0], "invited"}}
			return r.Emit(t)
		},
	}
	cmd.Flags().StringVar(&note, "note", "", "optional note to include with the invitation")
	return cmd
}

func newConnectionAcceptCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "accept <invitation-urn>",
		Short: "Accept a pending invitation (or all incoming invitations with --all)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app := appFrom(cmd)
			if err := app.requireWritable(); err != nil {
				return err
			}
			if !all && len(args) != 1 {
				return usageErr("provide an invitation urn or use --all")
			}
			if all && len(args) == 1 {
				return usageErr("provide either an invitation urn or --all, not both")
			}
			cl, err := app.Client()
			if err != nil {
				return err
			}
			ctx := context.Background()

			var accepted []string
			if all {
				invs, err := cl.Invitations(ctx, true)
				if err != nil {
					return err
				}
				for _, inv := range invs {
					if err := cl.AcceptInvitation(ctx, inv.InvitationURN, inv.SharedSecret); err != nil {
						return err
					}
					accepted = append(accepted, inv.InvitationURN)
				}
			} else {
				// Without the shared secret in hand, look the invitation up first.
				invs, err := cl.Invitations(ctx, true)
				if err != nil {
					return err
				}
				secret := ""
				for _, inv := range invs {
					if inv.InvitationURN == args[0] {
						secret = inv.SharedSecret
						break
					}
				}
				if err := cl.AcceptInvitation(ctx, args[0], secret); err != nil {
					return err
				}
				accepted = append(accepted, args[0])
			}

			r := app.Renderer()
			if app.Cfg.JSON {
				return r.Emit(map[string]any{"accepted": accepted})
			}
			t := &output.Table{Cols: []string{"INVITATION_URN", "STATUS"}}
			for _, urn := range accepted {
				t.Rows = append(t.Rows, []string{urn, "accepted"})
			}
			return r.Emit(t)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "accept all incoming invitations")
	return cmd
}

func newConnectionRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <id>",
		Short: "Remove an existing connection",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app := appFrom(cmd)
			if err := app.requireWritable(); err != nil {
				return err
			}
			cl, err := app.Client()
			if err != nil {
				return err
			}
			if err := cl.RemoveConnection(context.Background(), args[0]); err != nil {
				return err
			}
			r := app.Renderer()
			if app.Cfg.JSON {
				return r.Emit(map[string]string{"public_id": args[0], "status": "removed"})
			}
			t := &output.Table{Cols: []string{"PUBLIC_ID", "STATUS"}}
			t.Rows = [][]string{{args[0], "removed"}}
			return r.Emit(t)
		},
	}
}

func newConnectionRequestsCmd() *cobra.Command {
	var incoming, outgoing bool
	cmd := &cobra.Command{
		Use:   "requests",
		Short: "List pending connection requests",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app := appFrom(cmd)
			if incoming && outgoing {
				return usageErr("choose either --incoming or --outgoing, not both")
			}
			// Default to incoming, the common case.
			wantIncoming := !outgoing
			if outgoing {
				return fmt.Errorf("outgoing invitation listing is not yet supported")
			}
			cl, err := app.Client()
			if err != nil {
				return err
			}
			invs, err := cl.Invitations(context.Background(), wantIncoming)
			if err != nil {
				return err
			}
			r := app.Renderer()
			if app.Cfg.JSON {
				return r.Emit(invs)
			}
			t := &output.Table{Cols: []string{"INVITATION_URN", "FROM", "PUBLIC_ID", "MESSAGE"}}
			for _, inv := range invs {
				t.Rows = append(t.Rows, []string{inv.InvitationURN, inv.FromName, inv.FromPublicID, r.Untrusted(inv.Message)})
			}
			return r.Emit(t)
		},
	}
	cmd.Flags().BoolVar(&incoming, "incoming", false, "show incoming requests (default)")
	cmd.Flags().BoolVar(&outgoing, "outgoing", false, "show outgoing requests")
	return cmd
}
