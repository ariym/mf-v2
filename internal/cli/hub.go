package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/ariym/mf-v2/internal/config"
	"github.com/ariym/mf-v2/internal/hub"
	"github.com/ariym/mf-v2/internal/hubbrowser"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
)

func printHubTable(out io.Writer, kind string, page hub.Page) error {
	entries, err := hub.Entries(kind, page)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(out, 0, 4, 3, ' ', 0)
	fmt.Fprintln(w, "INSTALL NAME\tTITLE / TYPE\tDESCRIPTION")
	fmt.Fprintln(w, "------------\t------------\t-----------")
	for _, entry := range entries {
		fmt.Fprintf(w, "%s\t%s\t%s\n", entry.Name(), ansi.Truncate(entry.Title, 36, "…"), ansi.Truncate(entry.Description, 80, "…"))
	}
	if err = w.Flush(); err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Fprintln(out, "No definitions found.")
	}
	_, err = fmt.Fprintf(out, "\nInstall: mf %s install <install-name>\nPage: offset %d · %d items (limit %d)\n", hub.LocalKind(hub.WireKind(kind)), page.Offset, len(entries), page.Limit)
	return err
}

func hubBrowseCommand(c *config.Config) *cobra.Command {
	return &cobra.Command{Use: "browse [tagtypes|flows|programs]", Short: "Select hub definitions with Space and review installations with Enter", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		kind := ""
		if len(args) > 0 {
			kind = hub.WireKind(args[0])
			if !hub.ValidKind(kind) {
				return fmt.Errorf("kind must be tagtypes, flows, or programs")
			}
		}
		input, ok := cmd.InOrStdin().(*os.File)
		if !ok || !isatty.IsTerminal(input.Fd()) {
			return fmt.Errorf("hub browse requires an interactive terminal")
		}
		client := hub.Client{URL: c.HubURL, Token: os.Getenv("MF_HUB_ADMIN_TOKEN")}
		install := func(ctx context.Context, ref hub.Ref) error {
			if c.Path == "" {
				return fmt.Errorf("run mf init before installing hub definitions")
			}
			s, err := openStore(c)
			if err != nil {
				return err
			}
			defer s.Close()
			_, err = installer(c, s).Install(ctx, ref)
			return err
		}
		return hubbrowser.Run(cmd.Context(), client, kind, install, input, cmd.OutOrStdout())
	}}
}
