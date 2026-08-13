package cmd

import (
	"fmt"

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
	return &cobra.Command{
		Use:   "list",
		Short: "List data sources as ID<TAB>name",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			conn, err := resolveConnection(g.profile)
			if err != nil {
				return err
			}
			dss, err := redash.NewClient(conn.URL, conn.APIKey).ListDataSources(cmd.Context())
			if err != nil {
				return err
			}
			for _, ds := range dss {
				fmt.Fprintf(cmd.OutOrStdout(), "%d\t%s\n", ds.ID, ds.Name)
			}
			return nil
		},
	}
}
