package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAndCompatibility(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mf.yaml")
	c := Defaults()
	c.MFVersion = ""
	if e := Write(path, c); e != nil {
		t.Fatal(e)
	}
	got, e := Load(path)
	if e != nil {
		t.Fatal(e)
	}
	if got.MFVersion != Version || got.SQLitePath != filepath.Join(dir, "mf.db") {
		t.Fatalf("unexpected config: %+v", got)
	}
	if e = Write(path, Defaults()); e == nil {
		t.Fatal("overwrote existing config")
	}
	for _, v := range []string{"0.2.0", "1.1.0", "0.1.1", "broken", "0.01.0"} {
		if Compatible(v, "0.1.0") == nil {
			t.Errorf("accepted %s", v)
		}
	}
	if e = Compatible("0.1.0", "0.1.2"); e != nil {
		t.Fatal(e)
	}
	for _, body := range []string{"app_name: [", "unknown: true", "app_name: a\nserver_ip: localhost\nserver_port: 0\nsqlite_path: x", "---\n---\n"} {
		if e = os.WriteFile(path, []byte(body), 0600); e != nil {
			t.Fatal(e)
		}
		if _, e = Load(path); e == nil {
			t.Errorf("accepted %q", body)
		}
	}
	if _, e = Load(filepath.Join(dir, "absent")); e == nil || !strings.Contains(e.Error(), "absent") {
		t.Fatalf("missing explicit path: %v", e)
	}
}
func TestDiscoveryUsesWorkingDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, e := Load(""); e == nil || e.Error() != MissingMessage {
		t.Fatalf("got %v", e)
	}
	if e := Write("mf.yaml", Defaults()); e != nil {
		t.Fatal(e)
	}
	if _, e := Load(""); e != nil {
		t.Fatal(e)
	}
}

func TestLegacyConfigGetsNewDefaultsAndPartialRenderOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mf.yaml")
	body := "app_name: mf\nserver_ip: localhost\nserver_port: 11747\nsqlite_path: mf.db\nsql_to_ffmpeg:\n  crf: 0\n  width: 1280\n  height: 720\n"
	if e := os.WriteFile(path, []byte(body), 0600); e != nil {
		t.Fatal(e)
	}
	c, e := Load(path)
	if e != nil {
		t.Fatal(e)
	}
	if c.HubURL != "http://127.0.0.1:11748" || c.ModulesPath != filepath.Join(dir, "mf_modules") || c.SQLToFFmpeg.CRF != 0 || c.SQLToFFmpeg.FPS != 30 || c.SQLToFFmpeg.Width != 1280 {
		t.Fatalf("defaults/override %+v", c)
	}
}
