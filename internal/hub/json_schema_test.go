package hub

import (
	"encoding/json"
	"testing"
)

func TestStructuredTags(t *testing.T) {
	for _, tc := range []struct {
		name, schema string
		valid        bool
	}{
		{"object", `{"type":"object","properties":{"vector":{"type":"array","items":{"type":"number"}}}}`, true},
		{"local ref", `{"$defs":{"n":{"type":"number"}},"$ref":"#/$defs/n"}`, true},
		{"external ref", `{"$ref":"https://example.com/schema"}`, false},
		{"file ref", `{"$ref":"file:///etc/passwd"}`, false},
		{"nested external", `{"properties":{"a":{"$ref":"https://example.com/schema"}}}`, false},
		{"dynamic external", `{"$dynamicRef":"https://example.com/schema"}`, false},
		{"id", `{"$id":"https://example.com/schema"}`, false},
		{"old dialect", `{"$schema":"http://json-schema.org/draft-07/schema#"}`, false},
		{"invalid schema", `{"type":"not-a-type"}`, false},
		{"unresolved", `{"$ref":"#/$defs/missing"}`, false},
		{"boolean true root", `true`, true},
		{"boolean false root", `false`, true},
		{"boolean child", `{"additionalProperties":false}`, true},
		{"example is data", `{"examples":[{"$ref":"https://example.com"}]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"key":"rich","version":1,"data_type":"json","format":{"schema":` + tc.schema + `}}`)
			r, err := Decode(TagTypes, body)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
			if err == nil {
				encoded, _ := json.Marshal(r.Value)
				if _, err := Decode(TagTypes, encoded); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	for _, body := range []string{
		`{"key":"x","version":1,"data_type":"json"}`,
		`{"key":"x","version":1,"data_type":"json","format":{"schema":null}}`,
		`{"key":"x","version":1,"data_type":"json","format":{"schema":{},"choices":[1]}}`,
		`{"key":"x","version":1,"data_type":"string","format":{"schema":{}}}`,
		`{"key":"x","version":1,"data_type":"tag_set","format":{"schema":{},"tags":{"a":1}}}`,
	} {
		if _, err := Decode(TagTypes, []byte(body)); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}
