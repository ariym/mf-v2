// Package cli defines the mf command tree.
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ariym/mf-v2/internal/config"
	"github.com/ariym/mf-v2/internal/media"
	"github.com/ariym/mf-v2/internal/process"
	"github.com/ariym/mf-v2/internal/server"
	"github.com/ariym/mf-v2/internal/store"
	"github.com/spf13/cobra"
)

func New(in io.Reader, out, errOut io.Writer) *cobra.Command {
	var configPath string
	var c config.Config
	root := &cobra.Command{Use: "mf", Short: "Local media catalog and durable workflow server", Version: config.Version, SilenceUsage: true, SilenceErrors: true}
	root.SetIn(in)
	root.SetOut(out)
	root.SetErr(errOut)
	root.CompletionOptions.DisableDefaultCmd = true
	root.PersistentFlags().StringVar(&configPath, "config", "", "runtime config path (default: ./mf.yaml)")
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		var e error
		c, e = config.Load(configPath)
		if e != nil {
			return e
		}
		s, e := store.Open(c.SQLitePath)
		if e != nil {
			return e
		}
		return s.Close()
	}
	initCmd := &cobra.Command{Use: "init", Short: "Create mf.yaml and initialize its database", Args: cobra.NoArgs, PersistentPreRunE: func(*cobra.Command, []string) error { return nil }}
	var yes bool
	initCmd.Flags().BoolVarP(&yes, "yes", "y", false, "use all defaults")
	initCmd.RunE = func(cmd *cobra.Command, _ []string) error {
		return initialize(configPath, yes, cmd.InOrStdin(), cmd.OutOrStdout())
	}
	root.AddCommand(initCmd)
	version := &cobra.Command{Use: "version", Short: "Print executable version", Args: cobra.NoArgs, PersistentPreRunE: func(*cobra.Command, []string) error { return nil }, Run: func(cmd *cobra.Command, _ []string) { fmt.Fprintln(cmd.OutOrStdout(), config.Version) }}
	root.AddCommand(version)
	root.AddCommand(newUpdateCommand())
	add := &cobra.Command{Use: "add <file-or-directory>", Short: "Recursively probe and import local media", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		s, e := store.Open(c.SQLitePath)
		if e != nil {
			return e
		}
		defer s.Close()
		result, e := (&media.Importer{Store: s, ExcludeDirs: []string{c.ModulesPath}}).Import(cmd.Context(), args[0])
		fmt.Fprintf(cmd.OutOrStdout(), "Added: %d, updated: %d, skipped: %d, failed: %d\n", result.Added, result.Updated, result.Skipped, result.Failed)
		for _, failure := range result.Errors {
			fmt.Fprintln(cmd.ErrOrStderr(), failure)
		}
		return e
	}}
	root.AddCommand(add)
	srv := &cobra.Command{Use: "server", Short: "Run the API and job worker in this terminal"}
	srv.AddCommand(&cobra.Command{Use: "start", Short: "Start foreground server; interrupt to stop", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return server.Run(cmd.Context(), c, "", cmd.OutOrStdout()) }})
	root.AddCommand(srv)
	daemon := &cobra.Command{Use: "daemon", Short: "Manage the background server"}
	daemon.AddCommand(&cobra.Command{Use: "start", Short: "Start a detached server", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return process.Start(cmd.Context(), c, cmd.OutOrStdout()) }})
	var force bool
	stop := &cobra.Command{Use: "stop", Short: "Stop the daemon unless jobs are running", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return process.Stop(cmd.Context(), c, force, cmd.OutOrStdout())
	}}
	stop.Flags().BoolVarP(&force, "force", "f", false, "interrupt jobs; resume unfinished work on restart")
	daemon.AddCommand(stop)
	var follow bool
	logs := &cobra.Command{Use: "logs", Short: "Print the most recent 20 daemon log lines", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return process.Logs(cmd.Context(), c.StateDir(), follow, cmd.OutOrStdout())
	}}
	logs.Flags().BoolVar(&follow, "follow", false, "follow new log output until interrupted")
	daemon.AddCommand(logs)
	root.AddCommand(daemon)
	var instance string
	child := &cobra.Command{Use: "_daemon-run", Hidden: true, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return server.Run(cmd.Context(), c, instance, cmd.OutOrStdout())
	}}
	child.Flags().StringVar(&instance, "instance", "", "startup identity")
	root.AddCommand(child)
	addClientCommands(root, &c, &configPath)
	return root
}
func initialize(path string, yes bool, in io.Reader, out io.Writer) error {
	abs, e := config.ConfigPath(path)
	if e != nil {
		return e
	}
	if _, e = os.Lstat(abs); e == nil {
		return fmt.Errorf("config already exists at %s; refusing to overwrite", abs)
	} else if !os.IsNotExist(e) {
		return e
	}
	c := config.Defaults()
	if !yes {
		reader := bufio.NewReader(in)
		ask := func(question, def string) (string, error) {
			fmt.Fprintf(out, "%s [%s]: ", question, def)
			s, e := reader.ReadString('\n')
			if e != nil && !(errors.Is(e, io.EOF) && len(s) > 0) {
				return "", fmt.Errorf("read answer: %w; use mf init -y for defaults", e)
			}
			s = strings.TrimSpace(s)
			if s == "" {
				s = def
			}
			return s, nil
		}
		if c.AppName, e = ask("What is your server’s name?", c.AppName); e != nil {
			return e
		}
		if c.MFVersion, e = ask("version:", c.MFVersion); e != nil {
			return e
		}
		if c.ServerIP, e = ask("ip?", c.ServerIP); e != nil {
			return e
		}
		port, e := ask("port?", strconv.Itoa(c.ServerPort))
		if e != nil {
			return e
		}
		if c.ServerPort, e = strconv.Atoi(port); e != nil {
			return fmt.Errorf("server_port: expected an integer")
		}
		if c.SQLitePath, e = ask("Where is your SQLite.db file path?", c.SQLitePath); e != nil {
			return e
		}
	}
	if e = c.Validate(); e != nil {
		return e
	}
	dbPath := c.SQLitePath
	if !filepath.IsAbs(dbPath) {
		dbPath = filepath.Join(filepath.Dir(abs), dbPath)
	}
	s, e := store.Init(dbPath)
	if e != nil {
		return e
	}
	if e = s.Close(); e != nil {
		return e
	}
	if e = config.Write(abs, c); e != nil {
		return e
	}
	fmt.Fprintf(out, "Created %s and initialized database %s\n", abs, dbPath)
	return nil
}
func Execute(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) error {
	root := New(in, out, errOut)
	root.SetArgs(args)
	return root.ExecuteContext(ctx)
}
