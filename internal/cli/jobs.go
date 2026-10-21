package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"time"

	"github.com/ariym/mf-v2/internal/config"
	"github.com/ariym/mf-v2/internal/jobs"
	"github.com/ariym/mf-v2/internal/process"
	"github.com/spf13/cobra"
)

func apiRequest(ctx context.Context, c *config.Config, method, path string, body any, out any) error {
	var data []byte
	var e error
	if body != nil {
		data, e = json.Marshal(body)
		if e != nil {
			return e
		}
	}
	req, e := http.NewRequestWithContext(ctx, method, "http://"+c.Address()+path, bytes.NewReader(data))
	if e != nil {
		return e
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 30 * time.Minute}
	defer client.CloseIdleConnections()
	resp, e := client.Do(req)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var v struct{ Error string }
		if e = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v); e != nil {
			return fmt.Errorf("API HTTP %d", resp.StatusCode)
		}
		return fmt.Errorf("API HTTP %d: %s", resp.StatusCode, v.Error)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 128<<20)).Decode(out)
}
func ensureServer(cmd *cobra.Command, c *config.Config) error {
	var state process.State
	if e := process.Control(cmd.Context(), c.StateDir(), "GET", "/status", &state); e == nil && state.Ready {
		return nil
	}
	return process.Start(cmd.Context(), *c, cmd.ErrOrStderr())
}
func addJobCommands(root *cobra.Command, c *config.Config) {
	group := &cobra.Command{Use: "jobs", Short: "Run and manage durable jobs by file path"}
	for _, mode := range []string{"run", "submit"} {
		var input, outputDir string
		var wait bool
		use := mode + " <flow>"
		if mode == "run" {
			use += " [paths...]"
		}
		run := &cobra.Command{Use: use, Short: "Start the daemon if needed and submit a job", Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			if mode == "submit" && len(args) != 1 {
				return fmt.Errorf("submit accepts a flow and --input; use run for paths")
			}
			raw, e := jsonInput(input)
			if e != nil {
				return e
			}
			if outputDir != "" {
				var v map[string]json.RawMessage
				if e = json.Unmarshal(raw, &v); e != nil || v == nil {
					return fmt.Errorf("input must be an object")
				}
				abs, e := filepath.Abs(outputDir)
				if e != nil {
					return e
				}
				v["output_dir"], _ = json.Marshal(abs)
				raw, _ = json.Marshal(v)
			}
			body := map[string]any{"flow": args[0], "input": raw}
			if mode == "run" {
				paths := args[1:]
				if len(paths) == 0 {
					paths = []string{"."}
				}
				absolute := []string{}
				for _, path := range paths {
					abs, e := filepath.Abs(path)
					if e != nil {
						return e
					}
					absolute = append(absolute, abs)
				}
				body["paths"] = absolute
			}
			if e = ensureServer(cmd, c); e != nil {
				return e
			}
			var result struct {
				ID string `json:"id"`
			}
			if e = apiRequest(cmd.Context(), c, "POST", "/api/v1/jobs", body, &result); e != nil {
				return e
			}
			fmt.Fprintln(cmd.OutOrStdout(), result.ID)
			if wait {
				return waitJobCLI(cmd, c, result.ID)
			}
			return nil
		}}
		run.Flags().StringVar(&input, "input", "{}", "flow JSON input or @file")
		run.Flags().StringVar(&outputDir, "output-dir", "", "result output directory")
		run.Flags().BoolVar(&wait, "wait", false, "wait for terminal status")
		group.AddCommand(run)
	}
	var path, state string
	var limit, offset int
	list := &cobra.Command{Use: "list [path]", Short: "List jobs with optional media-path and state filters", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		filter := path
		if len(args) > 0 {
			filter = args[0]
		}
		if filter != "" {
			abs, e := filepath.Abs(filter)
			if e != nil {
				return e
			}
			filter = abs
		}
		q := url.Values{"limit": {fmt.Sprint(limit)}, "offset": {fmt.Sprint(offset)}}
		if filter != "" {
			q.Set("path", filter)
		}
		if state != "" {
			q.Set("state", state)
		}
		var out any
		if e := apiRequest(cmd.Context(), c, "GET", "/api/v1/jobs?"+q.Encode(), nil, &out); e != nil {
			return e
		}
		return printJSON(cmd.OutOrStdout(), out)
	}}
	list.Flags().StringVar(&path, "path", "", "filter by file or directory")
	list.Flags().StringVar(&state, "state", "", "filter by job state")
	list.Flags().IntVar(&limit, "limit", 100, "page size")
	list.Flags().IntVar(&offset, "offset", 0, "page offset")
	group.AddCommand(list)
	group.AddCommand(&cobra.Command{Use: "show <id>", Short: "Show job state, result and errors", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		var out any
		if e := apiRequest(cmd.Context(), c, "GET", "/api/v1/jobs/"+url.PathEscape(args[0]), nil, &out); e != nil {
			return e
		}
		return printJSON(cmd.OutOrStdout(), out)
	}})
	group.AddCommand(&cobra.Command{Use: "wait <id>", Short: "Wait for completion; fail if the job fails or is cancelled", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error { return waitJobCLI(cmd, c, args[0]) }})
	for _, action := range []string{"cancel", "retry"} {
		group.AddCommand(&cobra.Command{Use: action + " <id>", Short: action + " a durable job", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			if e := ensureServer(cmd, c); e != nil {
				return e
			}
			var out any
			if e := apiRequest(cmd.Context(), c, "POST", "/api/v1/jobs/"+url.PathEscape(args[0])+"/"+action, nil, &out); e != nil {
				return e
			}
			return printJSON(cmd.OutOrStdout(), out)
		}})
	}
	var follow bool
	var logOffset int64
	logs := &cobra.Command{Use: "logs <id>", Short: "Print captured flow diagnostics", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		offset := logOffset
		for {
			var out struct {
				Data string `json:"data"`
				Next int64  `json:"next_offset"`
			}
			if e := apiRequest(cmd.Context(), c, "GET", fmt.Sprintf("/api/v1/jobs/%s/logs?offset=%d", url.PathEscape(args[0]), offset), nil, &out); e != nil {
				return e
			}
			fmt.Fprint(cmd.OutOrStdout(), out.Data)
			offset = out.Next
			if !follow {
				return nil
			}
			if e := waitPoll(cmd.Context()); e != nil {
				return e
			}
		}
	}}
	logs.Flags().BoolVar(&follow, "follow", false, "follow logs until interrupted")
	logs.Flags().Int64Var(&logOffset, "offset", 0, "initial byte offset")
	group.AddCommand(logs)
	root.AddCommand(group)
}
func waitPoll(ctx context.Context) error {
	timer := time.NewTimer(300 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func waitJobCLI(cmd *cobra.Command, c *config.Config, id string) error {
	for {
		var j jobs.Job
		if e := apiRequest(cmd.Context(), c, "GET", "/api/v1/jobs/"+url.PathEscape(id), nil, &j); e != nil {
			return e
		}
		if j.State == "completed" {
			return printJSON(cmd.OutOrStdout(), j)
		}
		if j.State == "failed" || j.State == "cancelled" {
			_ = printJSON(cmd.OutOrStdout(), j)
			return fmt.Errorf("job %s %s: %s", id, j.State, j.Error)
		}
		if e := waitPoll(cmd.Context()); e != nil {
			return e
		}
	}
}
