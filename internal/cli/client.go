package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/ariym/mf-v2/internal/config"
	"github.com/ariym/mf-v2/internal/executable"
	"github.com/ariym/mf-v2/internal/hub"
	"github.com/ariym/mf-v2/internal/media"
	"github.com/ariym/mf-v2/internal/modules"
	"github.com/ariym/mf-v2/internal/process"
	"github.com/ariym/mf-v2/internal/store"
	"github.com/spf13/cobra"
)

func jsonInput(text string) (json.RawMessage, error) {
	raw := []byte(text)
	if len(text) > 0 && text[0] == '@' {
		f, e := os.Open(text[1:])
		if e != nil {
			return nil, e
		}
		defer f.Close()
		raw, e = io.ReadAll(io.LimitReader(f, 1<<20+1))
		if e != nil {
			return nil, e
		}
	}
	if len(raw) > 1<<20 {
		return nil, fmt.Errorf("JSON input exceeds 1 MiB")
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("input must be valid JSON")
	}
	return raw, nil
}
func numericID(text string) (int64, error) {
	id, e := strconv.ParseInt(text, 10, 64)
	if e != nil || id < 1 {
		return 0, fmt.Errorf("ID must be a positive integer")
	}
	return id, nil
}
func openStore(c *config.Config) (*store.Store, error) { return store.Open(c.SQLitePath) }
func installer(c *config.Config, s *store.Store) modules.Installer {
	return modules.Installer{Store: s, Root: c.ModulesPath, Hub: hub.Client{URL: c.HubURL, Token: os.Getenv("MF_HUB_ADMIN_TOKEN")}}
}
func addClientCommands(root *cobra.Command, c *config.Config, configPath *string) {
	migrate := &cobra.Command{Use: "migrate", Short: "Upgrade a validated database with the server stopped", Args: cobra.NoArgs, PersistentPreRunE: func(*cobra.Command, []string) error { var e error; *c, e = config.Load(*configPath); return e }, RunE: func(cmd *cobra.Command, _ []string) error {
		lock, e := process.Acquire(c.StateDir())
		if e != nil {
			return e
		}
		defer lock.Close()
		if e = store.Migrate(cmd.Context(), c.SQLitePath); e != nil {
			return e
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Database schema is current (v3)")
		return nil
	}}
	root.AddCommand(migrate)
	addHubCommands(root, c, configPath)
	addModuleCommands(root, c)
	addAnnotationCommands(root, c)
	addJobCommands(root, c)
	addBrowseCommand(root, c)
	addRenderCommand(root, c)
}
func addHubCommands(root *cobra.Command, c *config.Config, configPath *string) {
	var hubURL string
	group := &cobra.Command{Use: "hub", Short: "Browse and publish mf-hub definitions", PersistentPreRunE: func(*cobra.Command, []string) error {
		cfg, e := config.Load(*configPath)
		if e != nil {
			if *configPath != "" || e.Error() != config.MissingMessage {
				return e
			}
			cfg = config.Defaults()
		}
		if hubURL != "" {
			cfg.HubURL = hubURL
		}
		if e = cfg.Validate(); e != nil {
			return e
		}
		*c = cfg
		return nil
	}}
	group.PersistentFlags().StringVar(&hubURL, "url", "", "override hub URL")
	client := func() hub.Client { return hub.Client{URL: c.HubURL, Token: os.Getenv("MF_HUB_ADMIN_TOKEN")} }
	var limit, offset int
	list := &cobra.Command{Use: "list <tagtypes|flows|programs>", Short: "List latest definitions", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		p, e := client().List(cmd.Context(), args[0], "", limit, offset)
		if e != nil {
			return e
		}
		return printHubPage(cmd, args[0], p)
	}}
	list.Flags().IntVar(&limit, "limit", 100, "page size (1–1000)")
	list.Flags().IntVar(&offset, "offset", 0, "page offset")
	group.AddCommand(list)
	group.AddCommand(hubBrowseCommand(c))
	show := &cobra.Command{Use: "show <kind> <key[@version]>", Short: "Show a latest or exact definition", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		ref, e := hub.ParseRef(args[0], args[1])
		if e != nil {
			return e
		}
		record, e := client().Get(cmd.Context(), ref)
		if e != nil {
			return e
		}
		return printOutput(cmd, hub.ClientView(record.Value))
	}}
	group.AddCommand(show)
	var versionLimit, versionOffset int
	versions := &cobra.Command{Use: "versions <kind> <key>", Short: "List immutable version history", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		p, e := client().List(cmd.Context(), args[0], args[1], versionLimit, versionOffset)
		if e != nil {
			return e
		}
		return printHubPage(cmd, args[0], p)
	}}
	versions.Flags().IntVar(&versionLimit, "limit", 100, "page size")
	versions.Flags().IntVar(&versionOffset, "offset", 0, "page offset")
	group.AddCommand(versions)
	var osName, arch string
	resolve := &cobra.Command{Use: "resolve <flow[@version]>", Short: "Resolve exact dependencies for a platform", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		ref, e := hub.ParseRef(hub.Flows, args[0])
		if e != nil {
			return e
		}
		p, e := client().Resolve(cmd.Context(), ref, osName, arch)
		if e != nil {
			return e
		}
		return printOutput(cmd, hub.ClientView(p))
	}}
	resolve.Flags().StringVar(&osName, "os", "", "target OS (default: current)")
	resolve.Flags().StringVar(&arch, "arch", "", "target architecture (default: current)")
	group.AddCommand(resolve)
	group.AddCommand(&cobra.Command{Use: "publish <kind> <manifest.json>", Short: "Publish one immutable manifest using MF_HUB_ADMIN_TOKEN", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		raw, e := jsonInput("@" + args[1])
		if e != nil {
			return e
		}
		record, e := client().Publish(cmd.Context(), args[0], raw)
		if e != nil {
			return e
		}
		return printOutput(cmd, hub.ClientView(record.Value))
	}})
	group.AddCommand(&cobra.Command{Use: "health", Short: "Check hub readiness", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		out, e := client().Health(cmd.Context())
		if e != nil {
			return e
		}
		return printOutput(cmd, out)
	}})
	root.AddCommand(group)
}
func addModuleCommands(root *cobra.Command, c *config.Config) {
	for _, kind := range []string{"tagtypes", hub.Flows, hub.Programs} {
		group := &cobra.Command{Use: kind, Short: "Manage installed " + kind}
		group.AddCommand(&cobra.Command{Use: "list", Short: "List installed entities", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			s, e := openStore(c)
			if e != nil {
				return e
			}
			defer s.Close()
			if kind == "tagtypes" {
				v, e := s.TagTypes(cmd.Context())
				if e != nil {
					return e
				}
				return printOutput(cmd, v)
			}
			v, e := s.Modules(cmd.Context(), kind)
			if e != nil {
				return e
			}
			return printOutput(cmd, hub.ClientView(v))
		}})
		group.AddCommand(&cobra.Command{Use: "show <key[@version]>", Short: "Show an installed entity", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			ref, e := hub.ParseRef(kind, args[0])
			if e != nil {
				return e
			}
			s, e := openStore(c)
			if e != nil {
				return e
			}
			defer s.Close()
			if kind == "tagtypes" {
				v, e := s.Definition(cmd.Context(), ref.Identifier, ref.Version)
				if e != nil {
					return e
				}
				return printOutput(cmd, hub.ClientView(v))
			}
			v, e := s.Module(cmd.Context(), kind, ref.Identifier)
			if e != nil {
				return e
			}
			if ref.Version > 0 && ref.Version != v.Version {
				return fmt.Errorf("requested version is not installed")
			}
			return printOutput(cmd, hub.ClientView(v))
		}})
		var urlOverride string
		install := &cobra.Command{Use: "install <key[@version]>", Short: "Install exact compatible hub entities and dependencies", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			ref, e := hub.ParseRef(kind, args[0])
			if e != nil {
				return e
			}
			s, e := openStore(c)
			if e != nil {
				return e
			}
			defer s.Close()
			i := installer(c, s)
			if urlOverride != "" {
				i.Hub.URL = urlOverride
			}
			p, e := i.Install(cmd.Context(), ref)
			if e != nil {
				return e
			}
			return printOutput(cmd, hub.ClientView(p))
		}}
		install.Flags().StringVar(&urlOverride, "hub-url", "", "override hub URL")
		group.AddCommand(install)
		group.AddCommand(&cobra.Command{Use: "remove <key>", Short: "Remove an entity only when no dependencies or jobs require it", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			if !hub.ValidIdentifier(args[0]) {
				return fmt.Errorf("invalid identifier")
			}
			s, e := openStore(c)
			if e != nil {
				return e
			}
			defer s.Close()
			return installer(c, s).Remove(cmd.Context(), kind, args[0])
		}})
		if kind == "tagtypes" {
			group.AddCommand(&cobra.Command{Use: "add <manifest.json>", Short: "Add a local tag-type definition", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
				raw, e := jsonInput("@" + args[0])
				if e != nil {
					return e
				}
				record, e := hub.Decode(hub.TagTypes, raw)
				if e != nil {
					return e
				}
				s, e := openStore(c)
				if e != nil {
					return e
				}
				defer s.Close()
				if e = s.AddTagType(cmd.Context(), record.Value.(hub.TagType), "local"); e != nil {
					return e
				}
				return printOutput(cmd, hub.ClientView(record.Value))
			}})
		} else {
			group.AddCommand(&cobra.Command{Use: "path <key>", Short: "Print the installed executable's absolute path", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
				s, e := openStore(c)
				if e != nil {
					return e
				}
				defer s.Close()
				m, e := s.Module(cmd.Context(), kind, args[0])
				if e != nil {
					return e
				}
				if e = modules.Executable(m.Executable); e != nil {
					return e
				}
				fmt.Fprintln(cmd.OutOrStdout(), m.Executable)
				return nil
			}})
		}
		if kind == hub.Programs {
			var version int
			var formats string
			link := &cobra.Command{Use: "link <key> <executable>", Short: "Register an existing external program without imposing a CLI protocol", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
				var f []hub.MediaInputFormat
				if formats != "" {
					if e := json.Unmarshal([]byte(formats), &f); e != nil {
						return e
					}
				}
				s, e := openStore(c)
				if e != nil {
					return e
				}
				defer s.Close()
				return installer(c, s).Link(cmd.Context(), args[0], version, args[1], f)
			}}
			link.Flags().IntVar(&version, "version", 1, "local compatibility version")
			link.Flags().StringVar(&formats, "input-formats", "", "optional JSON input-format declarations")
			group.AddCommand(link)
			group.AddCommand(&cobra.Command{Use: "exec <key> -- [arguments...]", Short: "Run an installed program with ordinary CLI argument and I/O passthrough", Args: cobra.MinimumNArgs(1), DisableFlagParsing: true, RunE: func(cmd *cobra.Command, args []string) error {
				s, e := openStore(c)
				if e != nil {
					return e
				}
				m, e := s.Module(cmd.Context(), kind, args[0])
				s.Close()
				if e != nil {
					return e
				}
				if e = modules.Executable(m.Executable); e != nil {
					return e
				}
				rest := args[1:]
				if len(rest) > 0 && rest[0] == "--" {
					rest = rest[1:]
				}
				p := exec.CommandContext(cmd.Context(), m.Executable, rest...)
				p.Stdin = cmd.InOrStdin()
				p.Stdout = cmd.OutOrStdout()
				p.Stderr = cmd.ErrOrStderr()
				return executable.Run(cmd.Context(), p)
			}})
		}
		root.AddCommand(group)
	}
}
func importedFile(ctx context.Context, s *store.Store, path string, ensure bool) (store.Media, error) {
	abs, e := filepath.Abs(path)
	if e != nil {
		return store.Media{}, e
	}
	abs, e = filepath.EvalSymlinks(abs)
	if e != nil {
		return store.Media{}, e
	}
	if info, err := os.Stat(abs); err != nil {
		return store.Media{}, err
	} else if !info.Mode().IsRegular() {
		return store.Media{}, fmt.Errorf("expected a single regular media file: %s", path)
	}
	if ensure {
		if _, e = (&media.Importer{Store: s}).Import(ctx, abs); e != nil {
			return store.Media{}, e
		}
	}
	return s.ByPath(ctx, abs)
}
