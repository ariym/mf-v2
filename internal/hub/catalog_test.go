package hub

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestTagFormats(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"string", `{"key":"speech","version":1,"data_type":"string"}`, true},
		{"boolean", `{"key":"a","version":1,"data_type":"boolean","format":null}`, true},
		{"number range", `{"key":"a","version":1,"data_type":"number","format":{"range":[66,398]}}`, true},
		{"number labelled", `{"key":"a","version":1,"data_type":"number","format":{"range":[3,4],"choices":{"low":3,"high":4}}}`, true},
		{"string labelled", `{"key":"a","version":1,"data_type":"string","format":{"choices":{"label":"value"}}}`, true},
		{"string array", `{"key":"a","version":1,"data_type":"string","format":{"choices":["one","two"]}}`, true},
		{"number array", `{"key":"a","version":1,"data_type":"number","format":{"choices":[1,2.5]}}`, true},
		{"set", `{"key":"a","version":1,"data_type":"tag_set","format":{"tags":{"speech":1}}}`, true},
		{"old name", `{"key":"a","version":1,"data_type":"subtags"}`, false},
		{"range order", `{"key":"a","version":1,"data_type":"number","format":{"range":[3,1]}}`, false},
		{"range size", `{"key":"a","version":1,"data_type":"number","format":{"range":[1]}}`, false},
		{"null bound", `{"key":"a","version":1,"data_type":"number","format":{"range":[null,1]}}`, false},
		{"null range", `{"key":"a","version":1,"data_type":"number","format":{"range":null}}`, false},
		{"null tags", `{"key":"a","version":1,"data_type":"tag_set","format":{"tags":null}}`, false},
		{"string range", `{"key":"a","version":1,"data_type":"string","format":{"range":[1,2]}}`, false},
		{"range choice", `{"key":"a","version":1,"data_type":"number","format":{"range":[1,2],"choices":[3]}}`, false},
		{"mixed choices", `{"key":"a","version":1,"data_type":"string","format":{"choices":["one",2]}}`, false},
		{"null choice", `{"key":"a","version":1,"data_type":"number","format":{"choices":[null]}}`, false},
		{"empty choice", `{"key":"a","version":1,"data_type":"string","format":{"choices":[]}}`, false},
		{"boolean choice", `{"key":"a","version":1,"data_type":"boolean","format":{"choices":[true]}}`, false},
		{"no children", `{"key":"a","version":1,"data_type":"tag_set"}`, false},
		{"unversioned children", `{"key":"a","version":1,"data_type":"tag_set","format":{"tags":["speech"]}}`, false},
		{"bad pin", `{"key":"a","version":1,"data_type":"tag_set","format":{"tags":{"speech":0}}}`, false},
		{"mixed format", `{"key":"a","version":1,"data_type":"tag_set","format":{"tags":{"speech":1},"choices":["x"]}}`, false},
		{"primitive children", `{"key":"a","version":1,"data_type":"number","format":{"tags":{"speech":1}}}`, false},
		{"unknown", `{"key":"a","version":1,"data_type":"string","value":"instance"}`, false},
		{"unknown format", `{"key":"a","version":1,"data_type":"string","format":{"regex":".*"}}`, false},
		{"duplicate", `{"key":"a","key":"b","version":1,"data_type":"string"}`, false},
		{"trailing", `{"key":"a","version":1,"data_type":"string"} {}`, false},
		{"fraction version", `{"key":"a","version":1.1,"data_type":"string"}`, false},
		{"zero version", `{"key":"a","version":0,"data_type":"string"}`, false},
		{"invalid key", `{"key":"../a","version":1,"data_type":"string"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decode(TagTypes, []byte(tc.body))
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, err=%v", tc.valid, err)
			}
		})
	}
}
func asset() Artifact {
	return Artifact{"linux", "amd64", "https://github.com/example/repo/releases/download/v1/tool.tar.gz", strings.Repeat("a", 64), "bin/tool"}
}
func TestReleaseValidation(t *testing.T) {
	base := Program{
		ProgramName: "tool", Version: 1, Desc: "A tool",
		GitRepo:      "https://github.com/example/wrapper",
		InputFormats: []MediaInputFormat{{Container: ".mp4"}},
		Artifacts:    []Artifact{asset()},
	}
	for _, tc := range []struct {
		name   string
		change func(*Program)
		valid  bool
	}{
		{"valid", func(*Program) {}, true},
		{"no artifacts", func(p *Program) { p.Artifacts = nil }, false},
		{"duplicate platform", func(p *Program) { p.Artifacts = append(p.Artifacts, p.Artifacts[0]) }, false},
		{"checksum", func(p *Program) { p.Artifacts[0].SHA256 = "no" }, false},
		{"traversal", func(p *Program) { p.Artifacts[0].Entrypoint = "../bin/tool" }, false},
		{"absolute", func(p *Program) { p.Artifacts[0].Entrypoint = "/bin/tool" }, false},
		{"backslash", func(p *Program) { p.Artifacts[0].Entrypoint = `bin\tool` }, false},
		{"platform", func(p *Program) { p.Artifacts[0].OS = "windows" }, false},
		{"http", func(p *Program) { p.Artifacts[0].URL = "http://example.com/tool.tar.gz" }, false},
		{"credentials", func(p *Program) { p.GitRepo = "https://user:secret@github.com/a/b" }, false},
		{"archive", func(p *Program) { p.Artifacts[0].URL = "https://example.com/tool.zip" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			p.Artifacts = append([]Artifact{}, base.Artifacts...)
			tc.change(&p)
			b, _ := json.Marshal(p)
			_, err := Decode(Programs, b)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
	f := Flow{Key: "flow", Title: "Flow", Version: 1, Desc: "test", GHRepo: base.GitRepo, Artifacts: base.Artifacts}
	b, _ := json.Marshal(f)
	r, err := Decode(Flows, b)
	if err != nil {
		t.Fatal(err)
	}
	got := r.Value.(Flow)
	if got.TagTypes == nil || got.Flows == nil || got.Programs == nil {
		t.Fatal("nil dependency maps")
	}
}

func TestProgramMediaInputFormats(t *testing.T) {
	base := Program{
		ProgramName: "tool", Version: 1, Desc: "A tool",
		GitRepo: "https://github.com/example/wrapper", Artifacts: []Artifact{asset()},
		InputFormats: []MediaInputFormat{
			{Container: ".MP4"},
			{Container: ".mp4", VideoCodec: "H264"},
			{Container: ".WAV", AudioCodec: "PCM_S16LE", SampleRateHz: intPtr(16000)},
		},
	}
	b, _ := json.Marshal(base)
	r, err := Decode(Programs, b)
	if err != nil {
		t.Fatal(err)
	}
	got := r.Value.(Program).InputFormats
	if got[0].Container != ".mp4" || got[0].VideoCodec != "" || got[1].VideoCodec != "h264" || got[2].AudioCodec != "pcm_s16le" || *got[2].SampleRateHz != 16000 {
		t.Fatalf("formats were not preserved and normalized: %+v", got)
	}
	for _, tc := range []struct{ name, format string }{
		{"missing extension", `{}`},
		{"bare extension", `{"container":"mp4"}`},
		{"path", `{"container":"../mp4"}`},
		{"compound extension", `{"container":".tar.gz"}`},
		{"empty codec", `{"container":".mp4","video_codec":"h264 main"}`},
		{"blank video codec", `{"container":".mp4","video_codec":""}`},
		{"blank audio codec", `{"container":".wav","audio_codec":""}`},
		{"null audio codec", `{"container":".wav","audio_codec":null}`},
		{"null sample rate", `{"container":".wav","sample_rate_hz":null}`},
		{"zero rate", `{"container":".wav","sample_rate_hz":0}`},
		{"negative rate", `{"container":".wav","sample_rate_hz":-1}`},
		{"fractional rate", `{"container":".wav","sample_rate_hz":44100.5}`},
		{"unknown field", `{"container":".mp4","codec":"h264"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatal(err)
			}
			m["input_formats"] = []json.RawMessage{json.RawMessage(tc.format)}
			candidate, _ := json.Marshal(m)
			if _, err := Decode(Programs, candidate); err == nil {
				t.Fatalf("accepted %s", candidate)
			}
		})
	}
}

func intPtr(v int) *int { return &v }
func TestWalkGraphs(t *testing.T) {
	a, b, c := Ref{TagTypes, "a", 1}, Ref{TagTypes, "b", 1}, Ref{TagTypes, "c", 1}
	for _, tc := range []struct {
		name      string
		records   map[Ref]Record
		root      Ref
		order     []Ref
		errorText string
	}{
		{"shared", map[Ref]Record{a: {Ref: a, Dependencies: []Ref{b, c}}, b: {Ref: b, Dependencies: []Ref{c}}, c: {Ref: c}}, a, []Ref{c, b, a}, ""},
		{"cycle", map[Ref]Record{a: {Ref: a, Dependencies: []Ref{b}}, b: {Ref: b, Dependencies: []Ref{a}}}, a, nil, "cycle"},
		{"missing", map[Ref]Record{a: {Ref: a, Dependencies: []Ref{b}}}, a, nil, "missing"},
		{"conflict", map[Ref]Record{a: {Ref: a, Dependencies: []Ref{b, {TagTypes, "b", 2}}}, b: {Ref: b}, {TagTypes, "b", 2}: {Ref: Ref{TagTypes, "b", 2}}}, a, nil, "conflicting"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookup := func(_ context.Context, r Ref) (Record, error) {
				v, ok := tc.records[r]
				if !ok {
					return v, E("not_found", "missing %s", r)
				}
				return v, nil
			}
			ordered, err := Walk(context.Background(), tc.records[tc.root], lookup)
			if tc.errorText != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errorText) {
					t.Fatalf("expected %s, got %v", tc.errorText, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			refs := []Ref{}
			for _, r := range ordered {
				refs = append(refs, r.Ref)
			}
			if !reflect.DeepEqual(refs, tc.order) {
				t.Fatalf("%v != %v", refs, tc.order)
			}
		})
	}
}
