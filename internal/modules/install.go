// Package modules installs and pins executable hub entities. It never loads Go plugins.
package modules

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/ariym/mf-v2/internal/hub"
	"github.com/ariym/mf-v2/internal/store"
	"github.com/google/uuid"
)

type Installer struct {
	Store    *store.Store
	Root     string
	Hub      hub.Client
	Download func(context.Context, string, io.Writer) error
}
type installPath struct {
	Stage string `json:"stage"`
	Final string `json:"final"`
}

func (i *Installer) Lock() (func(), error) {
	if !filepath.IsAbs(i.Root) {
		return nil, fmt.Errorf("modules path must be absolute")
	}
	if e := os.MkdirAll(i.Root, 0700); e != nil {
		return nil, e
	}
	canonical, e := filepath.EvalSymlinks(i.Root)
	if e != nil {
		return nil, e
	}
	i.Root = canonical
	f, e := os.OpenFile(filepath.Join(i.Root, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); e != nil {
		f.Close()
		return nil, e
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}
func within(root, p string) bool {
	rel, e := filepath.Rel(root, p)
	return e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
func (i Installer) recover(ctx context.Context) error {
	rows, e := i.Store.DB.QueryContext(ctx, "SELECT id,paths FROM install_journal")
	if e != nil {
		return e
	}
	type journal struct{ id, raw string }
	var entries []journal
	for rows.Next() {
		var v journal
		if e = rows.Scan(&v.id, &v.raw); e != nil {
			rows.Close()
			return e
		}
		entries = append(entries, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, v := range entries {
		var paths []installPath
		if e = json.Unmarshal([]byte(v.raw), &paths); e != nil {
			return e
		}
		for _, p := range paths {
			if !within(i.Root, p.Stage) || !within(i.Root, p.Final) {
				return fmt.Errorf("invalid installation journal path")
			}
			if e = os.RemoveAll(p.Stage); e != nil {
				return e
			}
			var registered int
			if e = i.Store.DB.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM flow WHERE install_dir=?)+(SELECT count(*) FROM program WHERE install_dir=?)", p.Final, p.Final).Scan(&registered); e != nil {
				return e
			}
			if registered == 0 {
				if e = os.RemoveAll(p.Final); e != nil {
					return e
				}
			}
		}
		if _, e = i.Store.DB.ExecContext(ctx, "DELETE FROM install_journal WHERE id=?", v.id); e != nil {
			return e
		}
	}
	return nil
}
func (i Installer) Install(ctx context.Context, ref hub.Ref) (hub.Resolution, error) {
	plan, e := i.Hub.Resolve(ctx, ref, runtime.GOOS, runtime.GOARCH)
	if e != nil {
		return plan, e
	}
	unlock, e := i.Lock()
	if e != nil {
		return plan, e
	}
	defer unlock()
	if e = i.recover(ctx); e != nil {
		return plan, e
	}
	// Check the complete plan before downloading anything.
	var defs []hub.TagType
	for _, def := range plan.TagTypes {
		existing, e := i.Store.Definition(ctx, def.Key, 0)
		if e == nil {
			a, _ := json.Marshal(existing)
			b, _ := json.Marshal(def)
			if !store.SameJSON(a, b) {
				return plan, fmt.Errorf("tag type %s conflicts with installed version", def.Key)
			}
		} else if !errors.Is(e, sql.ErrNoRows) {
			return plan, e
		}
		defs = append(defs, def)
	}
	type pending struct {
		module   store.Module
		artifact hub.Artifact
	}
	var pendingModules []pending
	var all []store.Module
	for _, r := range plan.Order {
		if r.Kind == hub.TagTypes {
			continue
		}
		var manifest any
		var artifact hub.Artifact
		if r.Kind == hub.Programs {
			for _, v := range plan.Programs {
				if v.ProgramName == r.Identifier {
					manifest = v.Program
					artifact = v.Artifact
				}
			}
		} else {
			for _, v := range plan.Flows {
				if v.Key == r.Identifier {
					manifest = v.Flow
					artifact = v.Artifact
				}
			}
		}
		if manifest == nil {
			return plan, fmt.Errorf("incomplete installation plan")
		}
		raw, _ := json.Marshal(manifest)
		final := filepath.Join(i.Root, r.Kind, r.Identifier, fmt.Sprint(r.Version))
		m := store.Module{Kind: r.Kind, Key: r.Identifier, Version: r.Version, Manifest: raw, Source: i.Hub.URL, InstallDir: final, Executable: filepath.Join(final, filepath.FromSlash(artifact.Entrypoint)), OS: artifact.OS, Arch: artifact.Arch, ArtifactSHA256: artifact.SHA256}
		old, e := i.Store.Module(ctx, r.Kind, r.Identifier)
		if e == nil {
			if old.OS != "" && (old.OS != artifact.OS || old.Arch != artifact.Arch || old.ArtifactSHA256 != artifact.SHA256) {
				return plan, fmt.Errorf("%s %s was installed for a different platform or artifact", r.Kind, r.Identifier)
			}
			if old.Version != m.Version || !store.SameJSON(old.Manifest, m.Manifest) {
				return plan, fmt.Errorf("%s %s conflicts with installed version", r.Kind, r.Identifier)
			}
			if e = Executable(old.Executable); e != nil {
				return plan, fmt.Errorf("installed executable is unavailable: %w", e)
			}
			all = append(all, old)
			continue
		}
		if e != sql.ErrNoRows {
			return plan, e
		}
		all = append(all, m)
		pendingModules = append(pendingModules, pending{m, artifact})
	}
	paths := []installPath{}
	journalID := uuid.NewString()
	registered := false
	defer func() {
		if registered {
			return
		}
		for _, p := range paths {
			_ = os.RemoveAll(p.Stage)
			_ = os.RemoveAll(p.Final)
		}
		_, _ = i.Store.DB.ExecContext(context.Background(), "DELETE FROM install_journal WHERE id=?", journalID)
	}()
	// Record destination ownership before a rename can make a staged install visible.
	for _, p := range pendingModules {
		if _, e = os.Lstat(p.module.InstallDir); !os.IsNotExist(e) {
			return plan, fmt.Errorf("unregistered installation path exists: %s", p.module.InstallDir)
		}
		parent := filepath.Dir(p.module.InstallDir)
		if e = os.MkdirAll(parent, 0700); e != nil {
			return plan, e
		}
		stage := filepath.Join(parent, ".stage-"+journalID)
		paths = append(paths, installPath{stage, p.module.InstallDir})
	}
	if len(paths) > 0 {
		raw, _ := json.Marshal(paths)
		if _, e = i.Store.DB.ExecContext(ctx, "INSERT INTO install_journal(id,paths,created_at) VALUES(?,?,?)", journalID, string(raw), time.Now().UTC().Format(time.RFC3339Nano)); e != nil {
			return plan, e
		}
	}
	for n, p := range pendingModules {
		if e = os.Mkdir(paths[n].Stage, 0700); e != nil {
			return plan, e
		}
		archive, e := os.CreateTemp(i.Root, ".download-*")
		if e != nil {
			return plan, e
		}
		archiveName := archive.Name()
		download := i.Download
		if download == nil {
			download = hub.Download
		}
		hash := sha256.New()
		e = download(ctx, p.artifact.URL, io.MultiWriter(archive, hash))
		if e != nil {
			e = fmt.Errorf("download %s/%s@%d: %w", hub.LocalKind(p.module.Kind), p.module.Key, p.module.Version, e)
		}
		if e == nil && hex.EncodeToString(hash.Sum(nil)) != p.artifact.SHA256 {
			e = fmt.Errorf("SHA-256 mismatch for %s", p.module.Key)
		}
		if e == nil {
			_, e = archive.Seek(0, io.SeekStart)
		}
		if e == nil {
			e = Extract(ctx, archive, paths[n].Stage)
		}
		_ = archive.Close()
		_ = os.Remove(archiveName)
		if e != nil {
			return plan, e
		}
		entry := filepath.Join(paths[n].Stage, filepath.FromSlash(p.artifact.Entrypoint))
		if e = Executable(entry); e != nil {
			return plan, e
		}
		resolved, e := filepath.EvalSymlinks(entry)
		if e != nil || !within(paths[n].Stage, resolved) {
			return plan, fmt.Errorf("entrypoint escapes installation")
		}
	}
	for _, p := range paths {
		if e = os.Rename(p.Stage, p.Final); e != nil {
			return plan, e
		}
	}
	if e = i.Store.RegisterInstall(ctx, defs, all, journalID, i.Hub.URL); e != nil {
		return plan, e
	}
	registered = true
	return plan, nil
}
func Executable(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("executable path must be absolute")
	}
	st, e := os.Stat(path)
	if e != nil {
		return e
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0111 == 0 {
		return fmt.Errorf("not a regular executable: %s", path)
	}
	return nil
}

// Extract permits contained runtime links but rejects escaping links and writes
// through links. Hard links are materialized after all ordinary files exist.
func Extract(ctx context.Context, r io.Reader, root string) error {
	gz, e := gzip.NewReader(r)
	if e != nil {
		return e
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	type link struct {
		name, target string
		hard         bool
	}
	var links []link
	seen := map[string]bool{}
	var total int64
	count := 0
	for {
		if e = ctx.Err(); e != nil {
			return e
		}
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		count++
		if count > 200000 {
			return fmt.Errorf("archive exceeds entry limit")
		}
		name := strings.TrimSuffix(h.Name, "/")
		if name == "." {
			continue
		}
		if name == "" || path.IsAbs(name) || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") || strings.ContainsAny(name, "\\\x00") {
			return fmt.Errorf("unsafe archive path %q", h.Name)
		}
		if seen[name] {
			return fmt.Errorf("duplicate archive entry %q", name)
		}
		seen[name] = true
		dst := filepath.Join(root, filepath.FromSlash(name))
		if !within(root, dst) {
			return fmt.Errorf("archive path escapes root")
		}
		if e = os.MkdirAll(filepath.Dir(dst), 0700); e != nil {
			return e
		}
		switch h.Typeflag {
		case tar.TypeDir:
			e = os.MkdirAll(dst, 0755)
		case tar.TypeReg, tar.TypeRegA:
			if h.Size < 0 || h.Size > 32<<30-total {
				return fmt.Errorf("archive exceeds 32 GiB unpacked limit")
			}
			total += h.Size
			f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(h.Mode)&0755)
			if err != nil {
				return err
			}
			_, e = io.CopyN(f, tr, h.Size)
			ce := f.Close()
			if e == nil {
				e = ce
			}
		case tar.TypeSymlink, tar.TypeLink:
			target := h.Linkname
			if target == "" || path.IsAbs(target) || strings.ContainsAny(target, "\\\x00") {
				return fmt.Errorf("unsafe archive link")
			}
			resolved := filepath.Join(filepath.Dir(dst), filepath.FromSlash(target))
			if h.Typeflag == tar.TypeLink {
				resolved = filepath.Join(root, filepath.FromSlash(target))
			}
			if !within(root, resolved) {
				return fmt.Errorf("archive link escapes root")
			}
			links = append(links, link{dst, target, h.Typeflag == tar.TypeLink})
		default:
			return fmt.Errorf("unsupported archive entry type %d", h.Typeflag)
		}
		if e != nil {
			return e
		}
	}
	// Link parents must not already be links, including links just created here.
	for _, l := range links {
		parent, e := filepath.EvalSymlinks(filepath.Dir(l.name))
		if e != nil || !within(root, parent) {
			return fmt.Errorf("invalid archive link parent")
		}
		if l.hard {
			target := filepath.Join(root, filepath.FromSlash(l.target))
			st, e := os.Lstat(target)
			if e != nil || !st.Mode().IsRegular() {
				return fmt.Errorf("hard link target must be an existing regular file")
			}
			e = os.Link(target, l.name)
			if e != nil {
				return e
			}
		} else {
			if e = os.Symlink(l.target, l.name); e != nil {
				return e
			}
		}
	}
	for _, l := range links {
		resolved, e := filepath.EvalSymlinks(l.name)
		if e != nil || !within(root, resolved) {
			return fmt.Errorf("archive contains dangling, cyclic, or escaping link")
		}
	}
	return nil
}
