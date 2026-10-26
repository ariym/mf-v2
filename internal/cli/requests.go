package cli

import (
	"bufio"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/ariym/mf-v2/internal/config"
	"github.com/ariym/mf-v2/internal/hub"
	"github.com/ariym/mf-v2/internal/jobs"
	"github.com/spf13/cobra"
)

func requestAnswer(reader *bufio.Reader, out io.Writer, prompt string, yes bool) error {
	fmt.Fprint(out, prompt, " Y/n: ")
	if yes {
		fmt.Fprintln(out, "Y (--yes)")
		return nil
	}
	answer, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("approval requires an answer; use --yes for noninteractive acceptance: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "", "y", "yes":
		return nil
	case "n", "no":
		return fmt.Errorf("request declined")
	default:
		return fmt.Errorf("expected Y or n")
	}
}
func approveInstalls(cmd *cobra.Command, reader *bufio.Reader, p jobs.RequestPlan, yes bool) error {
	for _, item := range p.Installations {
		if err := requestAnswer(reader, cmd.ErrOrStderr(), fmt.Sprintf("%s/%s@%d needs to be installed to continue (download: %.2f MB).", hub.LocalKind(item.Ref.Kind), hub.DisplayText(item.Ref.Identifier), item.Ref.Version, float64(item.DownloadBytes)/1e6), yes); err != nil {
			return err
		}
	}
	for _, item := range p.Preparation {
		if item.Spec.License != "" {
			fmt.Fprintf(cmd.ErrOrStderr(), "Upstream license/access prerequisite: %s\n", hub.DisplayText(item.Spec.License))
		}
		if err := requestAnswer(reader, cmd.ErrOrStderr(), fmt.Sprintf("%s/%s preparation needs to be installed to continue (download: %.2f MB).", hub.DisplayText(item.Program), hub.DisplayText(item.Profile), float64(item.Spec.DownloadBytes)/1e6), yes); err != nil {
			return err
		}
	}
	return nil
}
func addRequestCommands(root *cobra.Command, c *config.Config) {
	var tags []string
	var segments []int64
	var flow, input string
	var background, dryRun, yes bool
	cmd := &cobra.Command{Use: "request <tagtype[@version]> [paths...]", Short: "Request media tags, reviewing installation and prerequisite jobs", Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		raw, err := jsonInput(input)
		if err != nil {
			return err
		}
		in := jobs.RequestInput{TagTypes: append([]string{args[0]}, tags...), SegmentIDs: segments, Flow: flow, Input: raw}
		for _, path := range args[1:] {
			abs, err := filepath.Abs(path)
			if err != nil {
				return err
			}
			in.Paths = append(in.Paths, abs)
		}
		if len(in.Paths) == 0 && len(segments) == 0 {
			return fmt.Errorf("supply media paths or --segment")
		}
		if err = ensureServer(cmd, c); err != nil {
			return err
		}
		reader := bufio.NewReader(cmd.InOrStdin())
		var plan jobs.RequestPlan
		for {
			if err = apiRequest(cmd.Context(), c, "POST", "/api/v1/requests/plan", in, &plan); err != nil {
				return err
			}
			if len(plan.Blockers) > 0 {
				if dryRun {
					return printOutput(cmd, plan)
				}
				if len(plan.Blockers) == 1 && strings.Contains(plan.Blockers[0], "ffprobe must be installed") {
					var bootstrap jobs.RequestPlan
					if err = apiRequest(cmd.Context(), c, "POST", "/api/v1/requests/bootstrap/plan", nil, &bootstrap); err != nil {
						return err
					}
					fmt.Fprintln(cmd.ErrOrStderr(), "FFprobe must inspect this media before the exact job plan can be reviewed.")
					if err = approveInstalls(cmd, reader, bootstrap, yes); err != nil {
						return err
					}
					if err = apiRequest(cmd.Context(), c, "POST", "/api/v1/requests/bootstrap", bootstrap, nil); err != nil {
						return err
					}
					continue
				}
				return fmt.Errorf("request blocked: %s", strings.Join(plan.Blockers, "; "))
			}
			if len(plan.Candidates) > 0 {
				if dryRun {
					return printOutput(cmd, plan)
				}
				keys := []string{}
				for key := range plan.Candidates {
					keys = append(keys, key)
				}
				sort.Strings(keys)
				if in.Producers == nil {
					in.Producers = map[string]string{}
				}
				for _, key := range keys {
					choices := plan.Candidates[key]
					fmt.Fprintf(cmd.ErrOrStderr(), "Choose a producer for %s:\n", hub.DisplayText(key))
					for i, name := range choices {
						fmt.Fprintf(cmd.ErrOrStderr(), "%d. %s\n", i+1, hub.DisplayText(name))
					}
					fmt.Fprint(cmd.ErrOrStderr(), "Producer number (or use --flow): ")
					line, x := reader.ReadString('\n')
					if x != nil {
						return fmt.Errorf("producer selection requires --flow or an interactive answer")
					}
					n, x := strconv.Atoi(strings.TrimSpace(line))
					if x != nil || n < 1 || n > len(choices) {
						return fmt.Errorf("invalid producer number")
					}
					in.Producers[key] = choices[n-1]
				}
				continue
			}
			break
		}
		if dryRun {
			return printOutput(cmd, plan)
		}
		if err = approveInstalls(cmd, reader, plan, yes); err != nil {
			return err
		}
		count := 0
		for _, prep := range plan.Preparation {
			count++
			fmt.Fprintf(cmd.ErrOrStderr(), "%d. Prepare %s@%d/%s\n", count, hub.DisplayText(prep.Program), prep.Version, hub.DisplayText(prep.Profile))
		}
		for _, node := range plan.Jobs {
			if node.Reused {
				fmt.Fprintf(cmd.ErrOrStderr(), "Reuse active job %s (%s)\n", node.ID, hub.DisplayText(node.Flow))
				continue
			}
			count++
			fmt.Fprintf(cmd.ErrOrStderr(), "%d. %s@%d — %s\n", count, hub.DisplayText(node.Flow), node.Version, hub.DisplayText(node.Data.Targets[0].MediaPath))
		}
		if count > 0 {
			if err = requestAnswer(reader, cmd.ErrOrStderr(), "Assign these jobs to continue?", yes); err != nil {
				return err
			}
		}
		var submitted struct {
			ID string `json:"id"`
		}
		if err = apiRequest(cmd.Context(), c, "POST", "/api/v1/requests", plan, &submitted); err != nil {
			return err
		}
		if background {
			return printOutput(cmd, submitted)
		}
		return waitRequestCLI(cmd, c, submitted.ID)
	}}
	cmd.Flags().StringSliceVar(&tags, "tagtype", nil, "additional requested tag type (repeatable)")
	cmd.Flags().Int64SliceVar(&segments, "segment", nil, "catalog segment IDs")
	cmd.Flags().StringVar(&flow, "flow", "", "explicit producer flow key@version")
	cmd.Flags().StringVar(&input, "input", "{}", "flow JSON options or @file")
	cmd.Flags().BoolVar(&background, "background", false, "return a request ID without waiting")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview without installing or submitting")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "accept installation and job approvals")
	root.AddCommand(cmd)
	group := &cobra.Command{Use: "requests", Short: "Inspect requested media outputs"}
	group.AddCommand(&cobra.Command{Use: "show <id>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		var out jobs.MediaRequest
		if err := apiRequest(cmd.Context(), c, "GET", "/api/v1/requests/"+url.PathEscape(args[0]), nil, &out); err != nil {
			return err
		}
		return printOutput(cmd, out)
	}})
	group.AddCommand(&cobra.Command{Use: "wait <id>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error { return waitRequestCLI(cmd, c, args[0]) }})
	root.AddCommand(group)
}
func waitRequestCLI(cmd *cobra.Command, c *config.Config, id string) error {
	for {
		var out jobs.MediaRequest
		if err := apiRequest(cmd.Context(), c, "GET", "/api/v1/requests/"+url.PathEscape(id), nil, &out); err != nil {
			return err
		}
		if out.State == "completed" {
			return printOutput(cmd, out.Results)
		}
		if out.State == "failed" {
			return fmt.Errorf("request %s failed: %s", id, out.Error)
		}
		if err := waitPoll(cmd.Context()); err != nil {
			return fmt.Errorf("%w; request continues as %s", err, id)
		}
	}
}
