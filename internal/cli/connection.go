package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/jodok/lion/internal/output"
	"github.com/jodok/lion/internal/voyager"
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
		Args:  usageArgs(cobra.NoArgs),
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
			return renderConnections(app.Renderer(), app.Cfg.JSON, conns)
		},
	}
}

// renderConnections wraps every LinkedIn-controlled free-text field (via
// wrapConnection, the single place that lists which Connection fields are
// free text — see untrusted.go) once and renders it identically for every
// output format — JSON included — so --wrap-untrusted is honored
// consistently (F17) rather than only in the table path, and rather than
// only some fields.
func renderConnections(r *output.Renderer, jsonOut bool, conns []voyager.Connection) error {
	for i := range conns {
		conns[i] = wrapConnection(r, conns[i])
	}
	if jsonOut {
		return r.Emit(conns)
	}
	t := &output.Table{Cols: []string{"PUBLIC_ID", "NAME", "HEADLINE"}}
	for _, c := range conns {
		t.Rows = append(t.Rows, []string{c.PublicID, c.Name, c.Headline})
	}
	return r.Emit(t)
}

func newConnectionInviteCmd() *cobra.Command {
	var note string
	cmd := &cobra.Command{
		Use:   "invite <id>",
		Short: "Send a connection invitation",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			app := appFrom(cmd)
			if err := app.requireWritable(); err != nil {
				return err
			}
			cl, err := app.Client()
			if err != nil {
				return err
			}
			id := args[0]
			r := app.Renderer()

			if !cl.DryRun() {
				ok, err := app.confirm(fmt.Sprintf("About to send a connection invite to %s. Proceed?", id))
				if err != nil {
					return err
				}
				if !ok {
					fmt.Fprintln(os.Stderr, "aborted: invitation not sent")
					return nil
				}
			}

			if err := cl.Invite(context.Background(), id, note); err != nil {
				return err
			}
			status := mutationStatus(cl.DryRun(), "invited")
			if app.Cfg.JSON {
				return r.Emit(map[string]string{"public_id": id, "status": status, "note": note})
			}
			t := &output.Table{Cols: []string{"PUBLIC_ID", "STATUS", "NOTE"}}
			t.Rows = [][]string{{id, status, note}}
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
		Args:  usageArgs(cobra.MaximumNArgs(1)),
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

			// Resolving which invitation(s) to accept is a read, so it
			// always runs — even under --dry-run — which is exactly what
			// lets dry-run show the real, specific invitation(s) it would
			// accept rather than a generic placeholder.
			var targets []voyager.Invitation
			if all {
				invs, err := cl.Invitations(ctx, true)
				if err != nil {
					return err
				}
				targets = invs
			} else {
				invs, err := cl.Invitations(ctx, true)
				if err != nil {
					return err
				}
				for _, inv := range invs {
					if inv.InvitationURN == args[0] {
						targets = append(targets, inv)
						break
					}
				}
				// Fail fast locally rather than sending an incomplete
				// mutation (an accept with an empty shared secret is known
				// to be invalid).
				if len(targets) == 0 || targets[0].SharedSecret == "" {
					return fmt.Errorf("no pending invitation with shared secret for %q: %w", args[0], voyager.ErrNotFound)
				}
			}

			r := app.Renderer()
			dryRun := cl.DryRun()
			if !dryRun && len(targets) > 0 {
				ok, err := app.confirm(fmt.Sprintf("About to accept %d invitation(s). Proceed?", len(targets)))
				if err != nil {
					return err
				}
				if !ok {
					fmt.Fprintln(os.Stderr, "aborted: no invitations accepted")
					return nil
				}
			}

			var accepted []string
			for _, inv := range targets {
				if !dryRun {
					if err := cl.AcceptInvitation(ctx, inv.InvitationURN, inv.SharedSecret); err != nil {
						return err
					}
				}
				accepted = append(accepted, inv.InvitationURN)
			}

			jsonVal, t := acceptResult(dryRun, accepted)
			if app.Cfg.JSON {
				return r.Emit(jsonVal)
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
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			app := appFrom(cmd)
			if err := app.requireWritable(); err != nil {
				return err
			}
			cl, err := app.Client()
			if err != nil {
				return err
			}
			id := args[0]
			r := app.Renderer()

			if !cl.DryRun() {
				ok, err := app.confirm(fmt.Sprintf("About to remove connection %s. Proceed?", id))
				if err != nil {
					return err
				}
				if !ok {
					fmt.Fprintln(os.Stderr, "aborted: connection not removed")
					return nil
				}
			}

			if err := cl.RemoveConnection(context.Background(), id); err != nil {
				return err
			}
			status := mutationStatus(cl.DryRun(), "removed")
			if app.Cfg.JSON {
				return r.Emit(map[string]string{"public_id": id, "status": status})
			}
			t := &output.Table{Cols: []string{"PUBLIC_ID", "STATUS"}}
			t.Rows = [][]string{{id, status}}
			return r.Emit(t)
		},
	}
}

func newConnectionRequestsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "requests",
		Short: "List pending incoming connection requests",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			app := appFrom(cmd)
			cl, err := app.Client()
			if err != nil {
				return err
			}
			invs, err := cl.Invitations(context.Background(), true)
			if err != nil {
				return err
			}
			return renderInvitations(app.Renderer(), app.Cfg.JSON, invs)
		},
	}
	// F26: outgoing invitation listing isn't implemented against a working
	// endpoint yet (DESIGN.md §3.2 — /relationships/invitationViews 400s),
	// so v1 only exposes the incoming direction. No --incoming/--outgoing
	// toggle is exposed at all rather than shipping a documented flag that
	// always errors.
	return cmd
}

// renderInvitations wraps every LinkedIn-controlled free-text field (via
// wrapInvitation) once and renders it identically for every output format
// (F17 — see renderConnections).
func renderInvitations(r *output.Renderer, jsonOut bool, invs []voyager.Invitation) error {
	for i := range invs {
		invs[i] = wrapInvitation(r, invs[i])
	}
	if jsonOut {
		return r.Emit(invs)
	}
	t := &output.Table{Cols: []string{"INVITATION_URN", "FROM", "PUBLIC_ID", "MESSAGE"}}
	for _, inv := range invs {
		t.Rows = append(t.Rows, []string{inv.InvitationURN, inv.FromName, inv.FromPublicID, inv.Message})
	}
	return r.Emit(t)
}

// mutationStatus returns the status label for a mutation command's output:
// "dry-run" whenever the client is in dry-run mode, regardless of the
// action, so automation never mistakes a preview for a completed mutation
// (F16); otherwise the given completed-state verb (e.g. "invited").
func mutationStatus(dryRun bool, verb string) string {
	if dryRun {
		return "dry-run"
	}
	return verb
}

// acceptResult builds the JSON value and table for `connection accept`.
//
// The live JSON contract keeps the original "accepted" field (the list of
// accepted invitation URNs) present and stable: an earlier dry-run rework
// replaced it with "invitations", which silently broke any automation
// reading the documented "accepted" field even though the mutation itself
// had succeeded. "status" is included alongside it, but "accepted" is the
// field with the load-bearing meaning and must always be present on a live
// run.
//
// Dry-run gets its own field name ("invitations", not "accepted") precisely
// so a preview can never be mistaken for confirmation that URNs were
// actually accepted — the same rule mutationStatus already enforces for the
// STATUS column/value.
func acceptResult(dryRun bool, accepted []string) (any, *output.Table) {
	status := mutationStatus(dryRun, "accepted")
	t := &output.Table{Cols: []string{"INVITATION_URN", "STATUS"}}
	for _, urn := range accepted {
		t.Rows = append(t.Rows, []string{urn, status})
	}
	if dryRun {
		return map[string]any{"status": status, "invitations": accepted}, t
	}
	return map[string]any{"status": status, "accepted": accepted}, t
}
