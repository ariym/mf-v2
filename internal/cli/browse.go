package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/ariym/mf-v2/internal/browser"
	"github.com/ariym/mf-v2/internal/config"
	"github.com/ariym/mf-v2/internal/process"
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
)

func addBrowseCommand(root *cobra.Command, c *config.Config) {
	root.AddCommand(&cobra.Command{Use: "browse [path]", Short: "Browse a live directory and right-to-left tag timelines", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		input, ok := cmd.InOrStdin().(*os.File)
		if !ok || !isatty.IsTerminal(input.Fd()) {
			return fmt.Errorf("browse requires an interactive terminal")
		}
		path := "."
		if len(args) > 0 {
			path = args[0]
		}
		s, e := openStore(c)
		if e != nil {
			return e
		}
		defer s.Close()
		run := func(ctx context.Context, flow, path string) (string, error) {
			var state process.State
			if e := process.Control(ctx, c.StateDir(), "GET", "/status", &state); e != nil || !state.Ready {
				if e = process.Start(ctx, *c, io.Discard); e != nil {
					return "", e
				}
			}
			var out struct {
				ID string `json:"id"`
			}
			e := apiRequest(ctx, c, "POST", "/api/v1/jobs", map[string]any{"flow": flow, "input": map[string]any{}, "paths": []string{path}}, &out)
			return out.ID, e
		}
		return browser.Run(cmd.Context(), s, path, input, cmd.OutOrStdout(), run, c.ModulesPath)
	}})
}
