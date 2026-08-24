package cmd

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/178inaba/rdsh/internal/redash"
)

func newDataSourceCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "data-source",
		Short: "Manage data sources",
	}
	cmd.AddCommand(newDataSourceListCmd(g))
	return cmd
}

func newDataSourceListCmd(g *globalFlags) *cobra.Command {
	var timeout timeoutFlag
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List data sources as ID<TAB>name",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDataSourceList(cmd, g, timeout.Duration())
		},
	}
	addTimeoutFlag(cmd, &timeout, serverTimeoutUsage)
	return cmd
}

func runDataSourceList(cmd *cobra.Command, g *globalFlags, timeout time.Duration) error {
	conn, err := resolveConnection(g.profile)
	if err != nil {
		return err
	}

	ctx, cancel := withTimeout(cmd.Context(), timeout)
	defer cancel()

	// A listing reads nothing into existence, so a deadline that cuts it off
	// leaves the server as it was and re-running is safe — which is what
	// exit code 124 tells an agent to do.
	dss, err := redash.NewClient(conn.URL, conn.APIKey).ListDataSources(ctx)
	if err != nil {
		return timeoutOr(err, timeout, "the data source listing")
	}

	for _, ds := range dss {
		fmt.Fprintf(cmd.OutOrStdout(), "%d\t%s\n", ds.ID, ds.Name)
	}
	return nil
}
