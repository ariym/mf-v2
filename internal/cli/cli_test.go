package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ariym/mf-v2/internal/config"
	"github.com/ariym/mf-v2/internal/store"
)

func TestInitDefaultsAndInteractive(t *testing.T) {
	for _, answers := range []string{"", "\n\n\n\n\n"} {
		t.Run(map[bool]string{true: "yes", false: "interactive"}[answers == ""], func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "mf.yaml")
			var out bytes.Buffer
			args := []string{"init", "--config", path}
			if answers == "" {
				args = append(args, "-y")
			}
			if e := Execute(context.Background(), args, strings.NewReader(answers), &out, &out); e != nil {
				t.Fatal(e)
			}
			c, e := config.Load(path)
			if e != nil {
				t.Fatal(e)
			}
			if c.AppName != "mf-processor" || c.ServerPort != 11747 {
				t.Fatalf("defaults: %+v", c)
			}
			s, e := store.Open(c.SQLitePath)
			if e != nil {
				t.Fatal(e)
			}
			s.Close()
			before, e := os.ReadFile(path)
			if e != nil {
				t.Fatal(e)
			}
			if e = Execute(context.Background(), args, strings.NewReader(answers), &out, &out); e == nil {
				t.Fatal("overwrote config")
			}
			after, e := os.ReadFile(path)
			if e != nil || string(before) != string(after) {
				t.Fatal("changed existing config")
			}
		})
	}
}
func TestConfigExemptions(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, args := range [][]string{{"--help"}, {"version"}, {"--version"}, {"daemon", "--help"}} {
		var out bytes.Buffer
		if e := Execute(context.Background(), args, strings.NewReader(""), &out, &out); e != nil {
			t.Fatalf("%v: %v", args, e)
		}
	}
	var out bytes.Buffer
	if e := Execute(context.Background(), []string{"add", "x"}, strings.NewReader(""), &out, &out); e == nil || e.Error() != config.MissingMessage {
		t.Fatalf("missing config: %v", e)
	}
}
