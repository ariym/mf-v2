package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientRoutesAndTerminology(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "POST" {
			if r.Header.Get("Authorization") != "Bearer secret" {
				t.Error("missing publication auth")
			}
			w.WriteHeader(201)
			fmt.Fprint(w, `{"key":"set","version":1,"data_type":"tag_set","format":{"tags":{"child":1}}}`)
			return
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("read sent admin token")
		}
		fmt.Fprint(w, `{"items":[{"key":"set","version":1,"data_type":"tag_set","format":{"tags":{"child":1}}}],"limit":5,"offset":2}`)
	}))
	defer server.Close()
	c := Client{URL: server.URL, Token: "secret"}
	p, e := c.List(context.Background(), "tagtypes", "", 5, 2)
	if e != nil {
		t.Fatal(e)
	}
	view, _ := json.Marshal(ClientView(p))
	if strings.Contains(string(view), `"tags"`) || !strings.Contains(string(view), `"tag_types"`) {
		t.Fatalf("client semantics: %s", view)
	}
	if _, e = c.List(context.Background(), "programs", "tool", 5, 2); e != nil {
		t.Fatal(e)
	}
	if _, e = c.Publish(context.Background(), "tagtypes", []byte(`{"key":"set","version":1,"data_type":"tag_set","format":{"tags":{"child":1}}}`)); e != nil {
		t.Fatal(e)
	}
	want := []string{"GET /api/v1/tags?limit=5&offset=2", "GET /api/v1/programs/tool/versions?limit=5&offset=2", "POST /api/v1/tags"}
	for n, path := range want {
		if paths[n] != path {
			t.Fatalf("%q != %q", paths[n], path)
		}
	}
}
func TestMediaFixtureSchemasAndValues(t *testing.T) {
	files, e := filepath.Glob("testdata/*.json")
	if e != nil || len(files) != 7 {
		t.Fatalf("fixtures: %v %v", files, e)
	}
	for _, file := range files {
		raw, e := os.ReadFile(file)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = Decode(TagTypes, raw); e != nil {
			t.Fatalf("%s: %v", file, e)
		}
	}
	r, e := Decode(TagTypes, []byte(`{"key":"rich","version":1,"data_type":"json","format":{"schema":{"$defs":{"n":{"type":"number","minimum":0}},"type":"object","properties":{"score":{"$ref":"#/$defs/n"}},"required":["score"],"additionalProperties":false}}}`))
	if e != nil {
		t.Fatal(e)
	}
	def := r.Value.(TagType)
	if e = ValidateValue(def, json.RawMessage(`{"score":0.8}`), nil); e != nil {
		t.Fatal(e)
	}
	for _, raw := range []string{`{"score":-1}`, `{"score":"bad"}`, `{}`, `{"score":1,"extra":true}`} {
		if e = ValidateValue(def, json.RawMessage(raw), nil); e == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
func TestResolveRejectsTamperedPlan(t *testing.T) {
	f := Flow{Key: "test", Title: "Test", Version: 1, Desc: "Test", Programs: Versions{}, Flows: Versions{}, TagTypes: Versions{}, GHRepo: "https://github.com/example/flow", Artifacts: []Artifact{asset()}}
	raw, _ := json.Marshal(f)
	root, e := Decode(Flows, raw)
	if e != nil {
		t.Fatal(e)
	}
	plan, e := Resolve(context.Background(), root, nil, "linux", "amd64")
	if e != nil {
		t.Fatal(e)
	}
	plan.Flows[0].Artifact.SHA256 = strings.Repeat("b", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/resolve") {
			json.NewEncoder(w).Encode(plan)
		} else {
			json.NewEncoder(w).Encode(f)
		}
	}))
	defer server.Close()
	if _, e = (Client{URL: server.URL}).Resolve(context.Background(), Ref{Flows, "test", 1}, "linux", "amd64"); e == nil {
		t.Fatal("accepted tampered resolution")
	}
}

func TestInstanceNumbersAndSchemaAnnotationsArePreserved(t *testing.T) {
	r, e := Decode(TagTypes, []byte(`{"key":"n","version":1,"data_type":"number","format":{"range":[0,1],"choices":[1]}}`))
	if e != nil {
		t.Fatal(e)
	}
	def := r.Value.(TagType)
	if e = ValidateValue(def, json.RawMessage(`1.0`), nil); e != nil {
		t.Fatal(e)
	}
	for _, raw := range []string{`1.00000000000000000000001`, `1e9999999999`, `-1e-9999999999`} {
		if ValidateValue(def, json.RawMessage(raw), nil) == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	r, e = Decode(TagTypes, []byte(`{"key":"large","version":1,"data_type":"json","format":{"schema":{"type":"integer","minimum":9007199254740993}}}`))
	if e != nil {
		t.Fatal(e)
	}
	if e = ValidateValue(r.Value.(TagType), json.RawMessage(`9007199254740992`), nil); e == nil {
		t.Fatal("lost integer precision")
	}
	if e = ValidateValue(r.Value.(TagType), json.RawMessage(`9007199254740993`), nil); e != nil {
		t.Fatal(e)
	}
	r, e = Decode(TagTypes, []byte(`{"key":"label","version":1,"data_type":"string","format":{"choices":{"tags":"a","kind":"b"}}}`))
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(ClientView(r.Value))
	if !strings.Contains(string(raw), `"tags":"a"`) {
		t.Fatalf("renamed data: %s", raw)
	}
	r, e = Decode(TagTypes, []byte(`{"key":"object","version":1,"data_type":"json","format":{"schema":{"type":"object"}}}`))
	if e != nil {
		t.Fatal(e)
	}
	if ValidateValue(r.Value.(TagType), json.RawMessage(`{"a":1,"a":2}`), nil) == nil {
		t.Fatal("accepted duplicate instance keys")
	}
}
