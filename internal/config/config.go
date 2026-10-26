// Package config handles local, explicit runtime configuration.
package config

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

var Version = "0.1.0"

// TestingRelease identifies an opt-in native build without changing schema compatibility.
var TestingRelease string

func DisplayVersion() string {
	if TestingRelease != "" {
		return Version + " (" + TestingRelease + ")"
	}
	return Version
}

const MissingMessage = "No local mf.yaml file found. include flag ‘--config=/path/to/mf.yaml’ or ‘run mf init’ to create a new mf.yaml in the current directory."

type Config struct {
	AppName       string       `yaml:"app_name" json:"app_name"`
	MFVersion     string       `yaml:"mf_version" json:"mf_version"`
	TestingBranch bool         `yaml:"testing_branch" json:"testing_branch"`
	ServerIP      string       `yaml:"server_ip" json:"server_ip"`
	ServerPort    int          `yaml:"server_port" json:"server_port"`
	SQLitePath    string       `yaml:"sqlite_path" json:"sqlite_path"`
	HubURL        string       `yaml:"hub_url" json:"hub_url"`
	ModulesPath   string       `yaml:"modules_path" json:"modules_path"`
	SQLToFFmpeg   RenderConfig `yaml:"sql_to_ffmpeg" json:"sql_to_ffmpeg"`
	Path          string       `yaml:"-" json:"-"`
}

func Defaults() Config {
	return Config{AppName: "mf-processor", MFVersion: Version, ServerIP: "localhost", ServerPort: 11747, SQLitePath: "mf.db", HubURL: "http://127.0.0.1:11748", ModulesPath: "mf_modules", SQLToFFmpeg: DefaultRender()}
}
func ConfigPath(path string) (string, error) {
	if path == "" {
		path = "mf.yaml"
	}
	return filepath.Abs(path)
}

// UpdateTestingBranch reads only the update preference. Updating must work even
// when the database is unavailable or mf_version needs a newer executable.
func UpdateTestingBranch(path string) (bool, error) {
	explicit := path != ""
	abs, err := ConfigPath(path)
	if err != nil {
		return false, err
	}
	b, err := os.ReadFile(abs)
	if os.IsNotExist(err) && !explicit {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read config %s: %w", abs, err)
	}
	var preference struct {
		TestingBranch bool `yaml:"testing_branch"`
	}
	d := yaml.NewDecoder(bytes.NewReader(b))
	if err = d.Decode(&preference); err != nil {
		return false, fmt.Errorf("parse config %s: %w", abs, err)
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return false, fmt.Errorf("parse config %s: expected one YAML document", abs)
	}
	return preference.TestingBranch, nil
}
func Load(path string) (Config, error) {
	explicit := path != ""
	abs, err := ConfigPath(path)
	if err != nil {
		return Config{}, err
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) && !explicit {
			return Config{}, fmt.Errorf("%s", MissingMessage)
		}
		return Config{}, fmt.Errorf("read config %s: %w", abs, err)
	}
	var c Config
	c.HubURL = Defaults().HubURL
	c.ModulesPath = Defaults().ModulesPath
	c.SQLToFFmpeg = DefaultRender()
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	if err = d.Decode(&c); err != nil {
		return c, fmt.Errorf("parse config %s: %w", abs, err)
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return c, fmt.Errorf("parse config %s: expected one YAML document", abs)
	}
	c.Path = abs
	if c.MFVersion == "" {
		c.MFVersion = Version
	}
	if err = c.Validate(); err != nil {
		return c, fmt.Errorf("invalid config %s: %w", abs, err)
	}
	if !filepath.IsAbs(c.SQLitePath) {
		c.SQLitePath = filepath.Join(filepath.Dir(abs), c.SQLitePath)
	}
	c.SQLitePath = filepath.Clean(c.SQLitePath)
	if !filepath.IsAbs(c.ModulesPath) {
		c.ModulesPath = filepath.Join(filepath.Dir(abs), c.ModulesPath)
	}
	// Existing symlink aliases must share the same daemon lock and database.
	if resolved, e := filepath.EvalSymlinks(c.SQLitePath); e == nil {
		c.SQLitePath = resolved
	}
	return c, nil
}
func (c Config) Validate() error {
	if strings.TrimSpace(c.AppName) == "" {
		return fmt.Errorf("app_name must not be empty")
	}
	if err := Compatible(c.MFVersion, Version); err != nil {
		return err
	}
	if strings.TrimSpace(c.ServerIP) == "" || strings.ContainsAny(c.ServerIP, "/ \t\n") {
		return fmt.Errorf("server_ip must be an IP address or hostname")
	}
	if c.ServerPort < 1 || c.ServerPort > 65535 {
		return fmt.Errorf("server_port must be between 1 and 65535")
	}
	if strings.TrimSpace(c.SQLitePath) == "" {
		return fmt.Errorf("sqlite_path must not be empty")
	}
	if c.HubURL != "" {
		u, e := url.Parse(c.HubURL)
		if e != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("hub_url must be an absolute HTTP(S) URL without credentials, query, or fragment")
		}
	}
	if c.ModulesPath == "" {
		return fmt.Errorf("modules_path must not be empty")
	}
	if err := c.SQLToFFmpeg.Validate(); err != nil {
		return err
	}
	return nil
}

var semver = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func Compatible(want, executable string) error {
	parse := func(s string) ([3]int, error) {
		var v [3]int
		m := semver.FindStringSubmatch(s)
		if m == nil {
			return v, fmt.Errorf("invalid version %q: expected major.minor.patch", s)
		}
		for i := range v {
			n, e := strconv.Atoi(m[i+1])
			if e != nil {
				return v, e
			}
			v[i] = n
		}
		return v, nil
	}
	w, e := parse(want)
	if e != nil {
		return fmt.Errorf("mf_version: %w", e)
	}
	v, e := parse(executable)
	if e != nil {
		return e
	}
	if w[0] != v[0] || w[1] != v[1] || w[2] > v[2] {
		return fmt.Errorf("mf_version %s is incompatible with executable %s (matching major/minor and no newer patch required)", want, executable)
	}
	return nil
}
func (c Config) Address() string  { return net.JoinHostPort(c.ServerIP, strconv.Itoa(c.ServerPort)) }
func (c Config) StateDir() string { return c.SQLitePath + ".mf" }
func Write(path string, c Config) error {
	b, e := yaml.Marshal(c)
	if e != nil {
		return e
	}
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return fmt.Errorf("create config %s (existing files are never overwritten): %w", path, e)
	}
	_, e = f.Write(b)
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		_ = os.Remove(path)
	}
	return e
}
