// Package hub defines immutable hub transport manifests and installation plans.
package hub

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
)

const (
	TagTypes = "tags"
	Flows    = "flows"
	Programs = "programs"
)

type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string                 { return e.Message }
func E(code, format string, args ...any) error { return &Error{code, fmt.Sprintf(format, args...)} }

type Ref struct {
	Kind       string `json:"kind"`
	Identifier string `json:"identifier"`
	Version    int    `json:"version"`
}

func (r Ref) String() string { return fmt.Sprintf("%s/%s@%d", r.Kind, r.Identifier, r.Version) }

type Versions map[string]int
type TagTypeFormat struct {
	Range    []float64       `json:"range,omitempty"`
	Choices  json.RawMessage `json:"choices,omitempty"`
	TagTypes Versions        `json:"tags,omitempty"`
	Schema   json.RawMessage `json:"schema,omitempty"`
}

func (f *TagTypeFormat) UnmarshalJSON(data []byte) error {
	type plain TagTypeFormat
	var value plain
	if err := DecodeStrict(data, &value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for key, raw := range fields {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("format.%s cannot be null; omit it instead", key)
		}
	}
	if raw, ok := fields["range"]; ok {
		var values []json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return err
		}
		for _, v := range values {
			if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
				return errors.New("range bounds must be numbers")
			}
		}
	}
	*f = TagTypeFormat(value)
	return nil
}

type TagType struct {
	Key      string         `json:"key"`
	Title    string         `json:"title,omitempty"`
	Desc     string         `json:"desc,omitempty"`
	DataType string         `json:"data_type"`
	Format   *TagTypeFormat `json:"format,omitempty"`
	Version  int            `json:"version"`
}
type Artifact struct {
	DownloadBytes *int64 `json:"download_bytes,omitempty"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	URL           string `json:"url"`
	SHA256        string `json:"sha256"`
	Entrypoint    string `json:"entrypoint"`
}
type Flow struct {
	Execution *Execution `json:"execution,omitempty"`
	Key       string     `json:"key"`
	Title     string     `json:"title"`
	Version   int        `json:"version"`
	Desc      string     `json:"desc"`
	Programs  Versions   `json:"programs"`
	Flows     Versions   `json:"flows"`
	TagTypes  Versions   `json:"tags"`
	GHRepo    string     `json:"gh_repo"`
	Artifacts []Artifact `json:"artifacts"`
}

// MediaInputFormat is one supported media input. Container names the file
// extension; omitted codec and sample-rate fields accept any value.
type MediaInputFormat struct {
	Container    string `json:"container"`
	VideoCodec   string `json:"video_codec,omitempty"`
	AudioCodec   string `json:"audio_codec,omitempty"`
	SampleRateHz *int   `json:"sample_rate_hz,omitempty"`
}

func (f *MediaInputFormat) UnmarshalJSON(data []byte) error {
	type plain MediaInputFormat
	var value plain
	if err := DecodeStrict(data, &value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for key, raw := range fields {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("input format %s cannot be null; omit it instead", key)
		}
	}
	if _, ok := fields["video_codec"]; ok && value.VideoCodec == "" {
		return errors.New("video_codec must not be empty; omit it to allow any video codec")
	}
	if _, ok := fields["audio_codec"]; ok && value.AudioCodec == "" {
		return errors.New("audio_codec must not be empty; omit it to allow any audio codec")
	}
	*f = MediaInputFormat(value)
	return nil
}

type Preparation struct {
	ReadyArgs     []string `json:"ready_args"`
	PrepareArgs   []string `json:"prepare_args"`
	DownloadBytes int64    `json:"download_bytes"`
	RequiredEnv   []string `json:"required_env,omitempty"`
	License       string   `json:"license,omitempty"`
}

type Program struct {
	Preparation  map[string]Preparation `json:"preparation,omitempty"`
	ProgramName  string                 `json:"program_name"`
	Version      int                    `json:"version"`
	Desc         string                 `json:"desc"`
	GitRepo      string                 `json:"git_repo"`
	InputFormats []MediaInputFormat     `json:"input_formats,omitempty"`
	Artifacts    []Artifact             `json:"artifacts"`
}
type Record struct {
	Ref          Ref
	Value        any
	Dependencies []Ref
}

var identifier = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,127}$`)
var digest = regexp.MustCompile(`^[a-f0-9]{64}$`)

func ValidIdentifier(s string) bool { return identifier.MatchString(s) }
func ValidKind(s string) bool       { return s == TagTypes || s == Flows || s == Programs }

// DecodeStrict also rejects duplicate object keys, which would otherwise silently
// overwrite dependency pins and make manifests ambiguous across clients.
func DecodeStrict(data []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := uniqueValue(d, 0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("expected exactly one JSON value")
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	d.DisallowUnknownFields()
	return d.Decode(dst)
}
func uniqueValue(d *json.Decoder, depth int) error {
	if depth > 64 {
		return errors.New("JSON nesting exceeds 64 levels")
	}
	t, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			s, ok := key.(string)
			if !ok {
				return errors.New("expected object key")
			}
			if seen[s] {
				return fmt.Errorf("duplicate field %q", s)
			}
			seen[s] = true
			if err := uniqueValue(d, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := uniqueValue(d, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	_, err = d.Token()
	return err
}

func Decode(kind string, data []byte) (Record, error) {
	var r Record
	var err error
	switch kind {
	case TagTypes:
		var v TagType
		if err = DecodeStrict(data, &v); err == nil {
			err = validateTagType(v)
		}
		r = Record{Ref: Ref{TagTypes, v.Key, v.Version}, Value: v}
		if v.Format != nil {
			r.Dependencies = refs(TagTypes, v.Format.TagTypes)
		}
	case Flows:
		var v Flow
		if err = DecodeStrict(data, &v); err == nil {
			if v.Programs == nil {
				v.Programs = Versions{}
			}
			if v.Flows == nil {
				v.Flows = Versions{}
			}
			if v.TagTypes == nil {
				v.TagTypes = Versions{}
			}
			err = validateRelease(v.Title, v.Desc, v.GHRepo, v.Artifacts, true)
		}
		r = Record{Ref: Ref{Flows, v.Key, v.Version}, Value: v}
		r.Dependencies = append(refs(TagTypes, v.TagTypes), refs(Programs, v.Programs)...)
		r.Dependencies = append(r.Dependencies, refs(Flows, v.Flows)...)
		r.Dependencies = append(r.Dependencies, executionDependencies(v.Execution)...)
		if err == nil {
			err = validateExecution(v.Execution, v.TagTypes)
		}
	case Programs:
		var v Program
		if err = DecodeStrict(data, &v); err == nil {
			err = validateRelease("", v.Desc, v.GitRepo, v.Artifacts, false)
			if err == nil {
				err = validateInputFormats(v.InputFormats)
				if err == nil {
					err = validatePreparation(v.Preparation)
				}
			}
		}
		r = Record{Ref: Ref{Programs, v.ProgramName, v.Version}, Value: v}
	default:
		return r, E("invalid_manifest", "unknown entity kind %q", kind)
	}
	if err != nil {
		return r, E("invalid_manifest", "%s", err)
	}
	if !ValidIdentifier(r.Ref.Identifier) || r.Ref.Version < 1 {
		return r, E("invalid_manifest", "identifier must match [a-z0-9][a-z0-9_-]{0,127} and version must be a positive integer")
	}
	for _, dep := range r.Dependencies {
		if !ValidIdentifier(dep.Identifier) || dep.Version < 1 {
			return r, E("invalid_manifest", "invalid dependency %s", dep)
		}
	}
	return r, nil
}
func refs(kind string, v Versions) []Ref {
	out := make([]Ref, 0, len(v))
	for k, n := range v {
		out = append(out, Ref{kind, k, n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Identifier < out[j].Identifier })
	return out
}
func validateTagType(t TagType) error {
	f := t.Format
	if t.DataType == "json" {
		if f == nil || len(f.Schema) == 0 || f.Range != nil || f.Choices != nil || f.TagTypes != nil {
			return errors.New("JSON tag types require format.schema and cannot have range, choices, or child tag types")
		}
		_, err := CompileTagTypeSchema(f.Schema)
		return err
	}
	if f != nil && f.Schema != nil {
		return errors.New("format.schema is only valid for JSON tag types")
	}
	if t.DataType == "tag_set" {
		if f == nil || len(f.TagTypes) == 0 || f.Range != nil || f.Choices != nil {
			return errors.New("tag_set requires format.tags and cannot have range or choices")
		}
		return nil
	}
	if t.DataType != "string" && t.DataType != "number" && t.DataType != "boolean" {
		return errors.New("data_type must be string, number, boolean, tag_set, or json")
	}
	if f == nil {
		return nil
	}
	if f.TagTypes != nil {
		return errors.New("format.tags is only valid for tag_set")
	}
	if f.Range != nil && (t.DataType != "number" || len(f.Range) != 2 || f.Range[0] > f.Range[1]) {
		return errors.New("range requires a number tag type and [minimum, maximum] with minimum <= maximum")
	}
	if f.Choices == nil {
		return nil
	}
	if t.DataType == "boolean" {
		return errors.New("boolean tag types cannot have choices")
	}
	var values []any
	var raw any
	if err := json.Unmarshal(f.Choices, &raw); err != nil {
		return err
	}
	switch c := raw.(type) {
	case []any:
		values = c
	case map[string]any:
		keys := make([]string, 0, len(c))
		for k := range c {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if strings.TrimSpace(k) == "" {
				return errors.New("choice labels must not be empty")
			}
			values = append(values, c[k])
		}
	default:
		return errors.New("choices must be a nonempty array or label-to-value object")
	}
	if len(values) == 0 {
		return errors.New("choices must not be empty")
	}
	for _, value := range values {
		if t.DataType == "string" {
			if _, ok := value.(string); !ok {
				return errors.New("string choices must contain only strings")
			}
		} else {
			n, ok := value.(float64)
			if !ok {
				return errors.New("number choices must contain only numbers")
			}
			if f.Range != nil && (n < f.Range[0] || n > f.Range[1]) {
				return errors.New("choice lies outside range")
			}
		}
	}
	return nil
}
func httpsURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Fragment == ""
}

var mediaContainer = regexp.MustCompile(`^\.[a-z0-9]+$`)
var mediaCodec = regexp.MustCompile(`^[a-z0-9][a-z0-9_.+-]*$`)

func validateInputFormats(formats []MediaInputFormat) error {
	for i := range formats {
		f := &formats[i]
		f.Container = strings.ToLower(f.Container)
		f.VideoCodec = strings.ToLower(f.VideoCodec)
		f.AudioCodec = strings.ToLower(f.AudioCodec)
		if !mediaContainer.MatchString(f.Container) {
			return fmt.Errorf("input_formats[%d].container must be a file extension such as .mp4 or .wav", i)
		}
		if f.VideoCodec != "" && !mediaCodec.MatchString(f.VideoCodec) {
			return fmt.Errorf("input_formats[%d].video_codec is invalid", i)
		}
		if f.AudioCodec != "" && !mediaCodec.MatchString(f.AudioCodec) {
			return fmt.Errorf("input_formats[%d].audio_codec is invalid", i)
		}
		if f.SampleRateHz != nil && *f.SampleRateHz <= 0 {
			return fmt.Errorf("input_formats[%d].sample_rate_hz must be positive", i)
		}
	}
	return nil
}
func ValidateInputFormats(formats []MediaInputFormat) error { return validateInputFormats(formats) }

func validateRelease(title, desc, repo string, assets []Artifact, needsTitle bool) error {
	if (needsTitle && strings.TrimSpace(title) == "") || strings.TrimSpace(desc) == "" {
		return errors.New("title (for flows) and desc must not be empty")
	}
	if !httpsURL(repo) {
		return errors.New("repository URL must be an absolute HTTPS URL")
	}
	if len(assets) == 0 {
		return errors.New("at least one artifact is required")
	}
	seen := map[string]bool{}
	for _, a := range assets {
		if a.DownloadBytes != nil && *a.DownloadBytes < 0 {
			return errors.New("download_bytes must be nonnegative")
		}
		if (a.OS != "darwin" && a.OS != "linux") || (a.Arch != "amd64" && a.Arch != "arm64") {
			return errors.New("artifacts support darwin/linux and amd64/arm64")
		}
		platform := a.OS + "/" + a.Arch
		if seen[platform] {
			return fmt.Errorf("duplicate artifact platform %s", platform)
		}
		seen[platform] = true
		u, err := url.Parse(a.URL)
		if err != nil || !httpsURL(a.URL) || !strings.HasSuffix(u.Path, ".tar.gz") {
			return errors.New("artifact URL must be an absolute HTTPS .tar.gz URL")
		}
		if !digest.MatchString(a.SHA256) {
			return errors.New("sha256 must be 64 lowercase hexadecimal characters")
		}
		if a.Entrypoint == "" || a.Entrypoint == "." || path.IsAbs(a.Entrypoint) || path.Clean(a.Entrypoint) != a.Entrypoint || strings.ContainsAny(a.Entrypoint, "\\\x00") || a.Entrypoint == ".." || strings.HasPrefix(a.Entrypoint, "../") {
			return errors.New("entrypoint must be a clean relative path within the archive")
		}
	}
	return nil
}

func validatePreparation(profiles map[string]Preparation) error {
	for name, p := range profiles {
		if !ValidIdentifier(name) || len(p.ReadyArgs) == 0 || len(p.PrepareArgs) == 0 || p.DownloadBytes < 0 {
			return errors.New("preparation requires a valid profile, ready_args, prepare_args and nonnegative download_bytes")
		}
		for _, name := range p.RequiredEnv {
			if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(name) {
				return errors.New("required_env must contain environment variable names")
			}
		}
		for _, args := range [][]string{p.ReadyArgs, p.PrepareArgs, p.RequiredEnv} {
			for _, a := range args {
				if strings.ContainsRune(a, 0) {
					return errors.New("preparation arguments cannot contain NUL")
				}
			}
		}
	}
	return nil
}

func (p *Preparation) UnmarshalJSON(raw []byte) error {
	type plain Preparation
	var value plain
	if err := DecodeStrict(raw, &value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	size, ok := fields["download_bytes"]
	if !ok || bytes.Equal(bytes.TrimSpace(size), []byte("null")) {
		return errors.New("preparation requires download_bytes")
	}
	*p = Preparation(value)
	return nil
}
