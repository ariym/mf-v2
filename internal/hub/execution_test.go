package hub

import (
	"context"
	"encoding/json"
	"testing"
)

func TestExecutionRequirementsAndProviderValidation(t *testing.T) {
	f := Flow{Key: "consumer", Version: 1, Title: "Consumer", Desc: "test", GHRepo: "https://github.com/example/flow", Artifacts: []Artifact{asset()}, TagTypes: Versions{"speech": 2}}
	f.Execution = &Execution{ProtocolVersion: 2, Requires: []Requirement{{Name: "transcript", AnyOf: []DataSpec{{Kind: "tag", Key: "speech", Version: 2}}, Producer: Producer{Flow: "producer", Version: 3}}}}
	raw, _ := json.Marshal(f)
	r, err := Decode(Flows, raw)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, dep := range r.Dependencies {
		if dep == (Ref{Flows, "producer", 3}) {
			found = true
		}
	}
	if !found {
		t.Fatal("producer not installed/pinned as a dependency")
	}
	provider := f
	provider.Key = "producer"
	provider.Version = 3
	provider.Execution = &Execution{ProtocolVersion: 2, Produces: []DataSpec{{Kind: "tag", Key: "speech", Version: 2}}}
	records := map[Ref]Record{{TagTypes, "speech", 2}: {Ref: Ref{TagTypes, "speech", 2}, Value: TagType{Key: "speech", Version: 2, DataType: "string"}}, {Flows, "producer", 3}: {Ref: Ref{Flows, "producer", 3}, Value: provider}}
	lookup := func(_ context.Context, ref Ref) (Record, error) { return records[ref], nil }
	if _, err = Walk(context.Background(), r, lookup); err != nil {
		t.Fatal(err)
	}
	provider.Execution.Produces = nil
	records[Ref{Flows, "producer", 3}] = Record{Ref: Ref{Flows, "producer", 3}, Value: provider}
	if _, err = Walk(context.Background(), r, lookup); err == nil {
		t.Fatal("accepted producer with missing output")
	}
	for _, change := range []func(*Execution){func(e *Execution) { e.ProtocolVersion = 1 }, func(e *Execution) { e.Requires[0].AnyOf[0].Version = 1 }, func(e *Execution) { e.Requires[0].Producer = Producer{Builtin: "unknown", Version: 1} }, func(e *Execution) { e.Requires = append(e.Requires, e.Requires[0]) }} {
		var copy Flow
		_ = json.Unmarshal(raw, &copy)
		change(copy.Execution)
		b, _ := json.Marshal(copy)
		if _, err = Decode(Flows, b); err == nil {
			t.Fatal("accepted invalid execution contract")
		}
	}
}
