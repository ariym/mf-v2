package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ariym/mf-v2/internal/config"
	"github.com/ariym/mf-v2/internal/render"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
)

type destinationPrompt struct {
	input       textinput.Model
	defaultPath string
	accepted    bool
}

func (p destinationPrompt) Init() tea.Cmd { return textinput.Blink }
func (p destinationPrompt) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if size, ok := msg.(tea.WindowSizeMsg); ok {
		p.input.Width = max(10, size.Width-2)
	}
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "enter":
			p.accepted = true
			return p, tea.Quit
		case "ctrl+c", "esc":
			return p, tea.Quit
		}
	}
	var cmd tea.Cmd
	p.input, cmd = p.input.Update(msg)
	return p, cmd
}
func (p destinationPrompt) View() string {
	return fmt.Sprintf("Save to [%s]\n%s\n", p.defaultPath, p.input.View())
}
func addRenderCommand(root *cobra.Command, c *config.Config) {
	var output string
	var printOnly, overwrite bool
	cmd := &cobra.Command{Use: "sql-to-ffmpeg <query>", Short: "Render selected segments; use --print-only to emit the command", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		s, e := openStore(c)
		if e != nil {
			return e
		}
		clips, e := render.Load(cmd.Context(), s, c.SQLitePath, args[0])
		s.Close()
		if e != nil {
			return e
		}
		destination := output
		if destination == "" {
			input, ok := cmd.InOrStdin().(*os.File)
			if !ok || !isatty.IsTerminal(input.Fd()) {
				return fmt.Errorf("--output is required without an interactive terminal")
			}
			ext := ".mp4"
			if render.AudioOnly(clips) {
				ext = ".mp3"
			}
			cwd, e := os.Getwd()
			if e != nil {
				return e
			}
			def := filepath.Join(cwd, time.Now().Format("20060102-150405.000000000")+ext)
			field := textinput.New()
			field.Width = 60
			field.Placeholder = def
			field.CharLimit = 4096
			field.Focus()
			p := destinationPrompt{input: field, defaultPath: def}
			result, e := tea.NewProgram(p, tea.WithInput(input), tea.WithOutput(cmd.ErrOrStderr()), tea.WithContext(cmd.Context())).Run()
			if e != nil {
				return e
			}
			chosen := result.(destinationPrompt)
			if !chosen.accepted {
				return context.Canceled
			}
			destination = chosen.input.Value()
			if destination == "" {
				destination = def
			}
		}
		command, e := render.Build(clips, destination, c.SQLToFFmpeg, overwrite)
		if e != nil {
			return e
		}
		fmt.Fprintln(cmd.OutOrStdout(), command.String())
		if printOnly {
			return nil
		}
		if e = command.Run(cmd.Context(), cmd.ErrOrStderr()); e != nil {
			return e
		}
		fmt.Fprintln(cmd.ErrOrStderr(), "Saved", command.Output)
		return nil
	}}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output path; required for noninteractive use")
	cmd.Flags().BoolVar(&printOnly, "print-only", false, "print the command without executing it")
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "allow replacing an existing output file")
	root.AddCommand(cmd)
}
