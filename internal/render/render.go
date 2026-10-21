// Package render turns selected media timestamps into a reproducible FFmpeg argv.
package render

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ariym/mf-v2/internal/config"
	"github.com/ariym/mf-v2/internal/executable"
	"github.com/ariym/mf-v2/internal/media"
	"github.com/ariym/mf-v2/internal/store"
)

type Clip struct {
	Media store.Media
	Start int64
	End   int64
}
type Command struct {
	Executable string   `json:"executable"`
	Args       []string `json:"args"`
	Output     string   `json:"output"`
}

func (c Command) String() string {
	args := append([]string{c.Executable}, c.Args...)
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", "'\"'\"'") + "'"
	}
	return strings.Join(quoted, " ")
}
func (c Command) Run(ctx context.Context, progress io.Writer) error {
	for _, arg := range c.Args {
		if arg == "-n" {
			if _, err := os.Stat(c.Output); err == nil {
				return fmt.Errorf("output exists; use --overwrite: %s", c.Output)
			} else if !os.IsNotExist(err) {
				return err
			}
			break
		}
	}
	cmd := exec.CommandContext(ctx, c.Executable, c.Args...)
	cmd.Stderr = progress
	cmd.Stdout = progress
	return executable.Run(ctx, cmd)
}
func AudioOnly(clips []Clip) bool {
	for _, clip := range clips {
		if clip.Media.MediaType == "video" {
			return false
		}
	}
	return true
}
func Load(ctx context.Context, s *store.Store, dbPath, query string) ([]Clip, error) {
	if err := SingleSelect(query); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: dbPath}
	q := u.Query()
	q.Set("mode", "ro")
	q.Add("_pragma", "query_only(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	index := map[string]int{}
	for n, column := range columns {
		key := strings.ToLower(column)
		if _, ok := index[key]; ok {
			return nil, fmt.Errorf("ambiguous result column %q; alias columns explicitly", column)
		}
		index[key] = n
	}
	_, startOK := index["start"]
	_, endOK := index["end"]
	_, pathOK := index["file_path"]
	_, idOK := index["media_file_id"]
	if !startOK || !endOK || (!pathOK && !idOK) {
		return nil, fmt.Errorf("query must return start, end, and media_file_id or file_path")
	}
	clips := []Clip{}
	cache := map[string]store.Media{}
	for rows.Next() {
		values := make([]any, len(columns))
		dest := make([]any, len(values))
		for i := range values {
			dest[i] = &values[i]
		}
		if err = rows.Scan(dest...); err != nil {
			return nil, err
		}
		text := func(name string) (string, error) {
			v := values[index[name]]
			switch x := v.(type) {
			case string:
				return x, nil
			case []byte:
				return string(x), nil
			}
			return "", fmt.Errorf("%s must be a timestamp or path string", name)
		}
		start, err := text("start")
		if err != nil {
			return nil, err
		}
		end, err := text("end")
		if err != nil {
			return nil, err
		}
		a, err := store.ParseTimecode(start)
		if err != nil {
			return nil, err
		}
		b, err := store.ParseTimecode(end)
		if err != nil {
			return nil, err
		}
		if b <= a {
			return nil, fmt.Errorf("clip end must be after start")
		}
		var file string
		if pathOK {
			file, err = text("file_path")
			if err != nil {
				return nil, err
			}
		} else {
			id, ok := values[index["media_file_id"]].(int64)
			if !ok || id < 1 {
				return nil, fmt.Errorf("media_file_id must be a positive integer")
			}
			m, err := s.Get(ctx, id)
			if err != nil {
				return nil, err
			}
			file = m.FilePath
		}
		file, err = filepath.Abs(file)
		if err != nil {
			return nil, err
		}
		file, err = filepath.EvalSymlinks(file)
		if err != nil {
			return nil, err
		}
		if strings.ContainsAny(file, "\x00\r\n") {
			return nil, fmt.Errorf("source paths must not contain NUL or newlines")
		}
		m, ok := cache[file]
		if !ok {
			m, err = (media.Probe{}).File(ctx, file)
			if err != nil {
				return nil, err
			}
			cache[file] = m
		}
		if m.Duration == nil || b > int64(*m.Duration*1e6+0.5) {
			return nil, fmt.Errorf("clip exceeds known duration of %s", file)
		}
		clips = append(clips, Clip{m, a, b})
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(clips) == 0 {
		return nil, fmt.Errorf("query returned no segments")
	}
	return clips, nil
}
func Build(clips []Clip, output string, c config.RenderConfig, overwrite bool) (Command, error) {
	out := Command{Executable: c.FFmpeg}
	if len(clips) == 0 {
		return out, fmt.Errorf("no clips selected")
	}
	if err := c.Validate(); err != nil {
		return out, err
	}
	audioOnly := AudioOnly(clips)
	ext := ".mp4"
	if audioOnly {
		ext = ".mp3"
	}
	if filepath.Ext(output) == "" {
		output += ext
	}
	if !strings.EqualFold(filepath.Ext(output), ext) {
		return out, fmt.Errorf("selected media requires %s output", ext)
	}
	abs, err := filepath.Abs(output)
	if err != nil {
		return out, err
	}
	if strings.ContainsAny(abs, "\x00\r\n") {
		return out, fmt.Errorf("output path must not contain NUL or newlines")
	}
	out.Output = abs
	flag := "-n"
	if overwrite {
		flag = "-y"
	}
	out.Args = []string{"-hide_banner", "-nostdin", flag}
	for _, clip := range clips {
		if clip.End <= clip.Start || clip.Start < 0 {
			return out, fmt.Errorf("invalid clip timestamps")
		}
		source, err := filepath.Abs(clip.Media.FilePath)
		if err != nil {
			return out, err
		}
		if strings.ContainsAny(source, "\x00\r\n") {
			return out, fmt.Errorf("source path contains newlines")
		}
		resolved := abs
		if p, err := filepath.EvalSymlinks(abs); err == nil {
			resolved = p
		}
		if source == resolved {
			return out, fmt.Errorf("output must not replace a source file")
		}
		out.Args = append(out.Args, "-ss", store.Timecode(clip.Start), "-t", seconds(clip.End-clip.Start), "-i", source)
	}
	layout := "stereo"
	if c.Channels == 1 {
		layout = "mono"
	}
	var filters []string
	var concat strings.Builder
	for n, clip := range clips {
		realVideo := []store.Video{}
		if clip.Media.MediaType == "video" {
			for _, v := range clip.Media.Video {
				if !v.AttachedPic {
					realVideo = append(realVideo, v)
				}
			}
		}
		clip.Media.Video = realVideo
		duration := seconds(clip.End - clip.Start)
		if !audioOnly {
			v := fmt.Sprintf("color=c=black:s=%dx%d:r=%d:d=%s", c.Width, c.Height, c.FPS, duration)
			if len(clip.Media.Video) > 0 {
				v = fmt.Sprintf("[%d:%d]trim=duration=%s,scale=%d:%d:force_original_aspect_ratio=decrease:force_divisible_by=2,pad=%d:%d:(ow-iw)/2:(oh-ih)/2", n, clip.Media.Video[0].StreamIndex, duration, c.Width, c.Height, c.Width, c.Height)
			}
			v += fmt.Sprintf(",setsar=1,fps=%d,format=%s,setpts=PTS-STARTPTS[v%d]", c.FPS, c.PixelFormat, n)
			filters = append(filters, v)
			fmt.Fprintf(&concat, "[v%d]", n)
		}
		a := fmt.Sprintf("anullsrc=r=%d:cl=%s", c.SampleRate, layout)
		if len(clip.Media.Audio) > 0 {
			a = fmt.Sprintf("[%d:%d]anull", n, clip.Media.Audio[0].StreamIndex)
		}
		a += fmt.Sprintf(",atrim=duration=%s,asetpts=PTS-STARTPTS,aresample=%d,aformat=sample_fmts=fltp:channel_layouts=%s,apad=whole_dur=%s,atrim=duration=%s[a%d]", duration, c.SampleRate, layout, duration, duration, n)
		filters = append(filters, a)
		fmt.Fprintf(&concat, "[a%d]", n)
	}
	streams := 1
	if audioOnly {
		streams = 0
	}
	fmt.Fprintf(&concat, "concat=n=%d:v=%d:a=1", len(clips), streams)
	if !audioOnly {
		concat.WriteString("[v]")
	}
	concat.WriteString("[a]")
	filters = append(filters, concat.String())
	out.Args = append(out.Args, "-filter_complex", strings.Join(filters, ";"))
	if audioOnly {
		out.Args = append(out.Args, "-map", "[a]", "-vn", "-c:a", c.MP3Codec, "-q:a", strconv.Itoa(c.MP3Quality), "-f", "mp3")
	} else {
		out.Args = append(out.Args, "-map", "[v]", "-map", "[a]", "-c:v", c.VideoCodec, "-preset", c.Preset, "-crf", strconv.Itoa(c.CRF), "-pix_fmt", c.PixelFormat, "-c:a", c.AudioCodec, "-b:a", c.AudioBitrate, "-movflags", "+faststart", "-f", "mp4")
	}
	out.Args = append(out.Args, "-ar", strconv.Itoa(c.SampleRate), "-ac", strconv.Itoa(c.Channels), "-map_metadata", "-1", "-map_chapters", "-1", out.Output)
	return out, nil
}
func seconds(us int64) string { return fmt.Sprintf("%d.%06d", us/1e6, us%1e6) }

// SingleSelect rejects statement chaining while respecting quoted text and SQL
// comments. SQLite query_only additionally rejects writes hidden behind WITH.
func SingleSelect(query string) error {
	first := ""
	ended := false
	for n := 0; n < len(query); {
		c := query[n]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			n++
			continue
		}
		if n+1 < len(query) && query[n:n+2] == "--" {
			n += 2
			for n < len(query) && query[n] != '\n' {
				n++
			}
			continue
		}
		if n+1 < len(query) && query[n:n+2] == "/*" {
			end := strings.Index(query[n+2:], "*/")
			if end < 0 {
				return fmt.Errorf("unterminated SQL comment")
			}
			n += end + 4
			continue
		}
		if ended {
			return fmt.Errorf("only one SELECT statement is allowed")
		}
		if c == ';' {
			ended = true
			n++
			continue
		}
		if c == '\'' || c == '"' || c == '`' || c == '[' {
			quote := c
			if quote == '[' {
				quote = ']'
			}
			n++
			closed := false
			for n < len(query) {
				if query[n] == quote {
					n++
					if n < len(query) && query[n] == quote && quote != ']' {
						n++
						continue
					}
					closed = true
					break
				}
				n++
			}
			if !closed {
				return fmt.Errorf("unterminated SQL quotation")
			}
			if first == "" {
				return fmt.Errorf("query must start with SELECT or WITH")
			}
			continue
		}
		if first == "" {
			start := n
			for n < len(query) && ((query[n] >= 'a' && query[n] <= 'z') || (query[n] >= 'A' && query[n] <= 'Z')) {
				n++
			}
			first = strings.ToUpper(query[start:n])
			if first != "SELECT" && first != "WITH" {
				return fmt.Errorf("query must be a read-only SELECT or WITH")
			}
			continue
		}
		n++
	}
	if first == "" {
		return fmt.Errorf("empty SQL query")
	}
	return nil
}
