package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
)

type Lookup func(context.Context, Ref) (Record, error)

// Walk returns each record once, dependencies first. It is also used during
// publication, before any database writes, to validate the entire graph.
func Walk(ctx context.Context, root Record, lookup Lookup) ([]Record, error) {
	state := map[Ref]int{}
	pins := map[string]Ref{}
	var ordered []Record
	var visit func(Record) error
	visit = func(r Record) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if state[r.Ref] == 1 {
			return E("invalid_dependencies", "dependency cycle at %s", r.Ref)
		}
		id := r.Ref.Kind + "/" + r.Ref.Identifier
		if old, ok := pins[id]; ok && old.Version != r.Ref.Version {
			return E("invalid_dependencies", "conflicting requirements: %s and %s", old, r.Ref)
		}
		if state[r.Ref] == 2 {
			return nil
		}
		if len(pins) >= 10000 {
			return E("invalid_dependencies", "dependency graph exceeds 10000 entities")
		}
		pins[id] = r.Ref
		state[r.Ref] = 1
		for _, dep := range r.Dependencies {
			if state[dep] == 1 {
				return E("invalid_dependencies", "dependency cycle: %s requires %s", r.Ref, dep)
			}
			next, err := lookup(ctx, dep)
			if err != nil {
				return err
			}
			if err = visit(next); err != nil {
				return err
			}
		}
		if err := validateProducers(r, ordered); err != nil {
			return err
		}
		state[r.Ref] = 2
		ordered = append(ordered, r)
		return nil
	}
	if err := visit(root); err != nil {
		return nil, err
	}
	return ordered, nil
}

type ResolvedProgram struct {
	Program
	Artifact Artifact `json:"artifact"`
}
type ResolvedFlow struct {
	Flow
	Artifact Artifact `json:"artifact"`
}
type Resolution struct {
	Root     Ref               `json:"root"`
	OS       string            `json:"os"`
	Arch     string            `json:"arch"`
	TagTypes []TagType         `json:"tags"`
	Programs []ResolvedProgram `json:"programs"`
	Flows    []ResolvedFlow    `json:"flows"`
	Order    []Ref             `json:"order"`
}

func Resolve(ctx context.Context, root Record, lookup Lookup, os, arch string) (Resolution, error) {
	out := Resolution{Root: root.Ref, OS: os, Arch: arch, TagTypes: []TagType{}, Programs: []ResolvedProgram{}, Flows: []ResolvedFlow{}, Order: []Ref{}}
	if (os != "linux" && os != "darwin") || (arch != "amd64" && arch != "arm64") {
		return out, E("invalid_request", "os must be linux or darwin; arch must be amd64 or arm64")
	}
	ordered, err := Walk(ctx, root, lookup)
	if err != nil {
		return out, err
	}
	selectArtifact := func(ref Ref, assets []Artifact) (Artifact, error) {
		for _, a := range assets {
			if a.OS == os && a.Arch == arch {
				return a, nil
			}
		}
		return Artifact{}, E("unsupported_platform", "%s has no artifact for %s/%s", ref, os, arch)
	}
	for _, r := range ordered {
		out.Order = append(out.Order, r.Ref)
		switch v := r.Value.(type) {
		case TagType:
			out.TagTypes = append(out.TagTypes, v)
		case Program:
			a, err := selectArtifact(r.Ref, v.Artifacts)
			if err != nil {
				return out, err
			}
			out.Programs = append(out.Programs, ResolvedProgram{v, a})
		case Flow:
			a, err := selectArtifact(r.Ref, v.Artifacts)
			if err != nil {
				return out, err
			}
			out.Flows = append(out.Flows, ResolvedFlow{v, a})
		}
	}
	return out, nil
}

// ValidateResolution verifies an offline, pinned installation plan against its manifests.
func ValidateResolution(ctx context.Context, plan Resolution) error {
	records := map[Ref]Record{}
	add := func(kind string, v any) error {
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		r, err := Decode(kind, raw)
		if err != nil {
			return err
		}
		if _, ok := records[r.Ref]; ok {
			return fmt.Errorf("duplicate resolution entity %s", r.Ref)
		}
		records[r.Ref] = r
		return nil
	}
	for _, v := range plan.TagTypes {
		if err := add(TagTypes, v); err != nil {
			return err
		}
	}
	for _, v := range plan.Programs {
		if err := add(Programs, v.Program); err != nil {
			return err
		}
	}
	for _, v := range plan.Flows {
		if err := add(Flows, v.Flow); err != nil {
			return err
		}
	}
	root, ok := records[plan.Root]
	if !ok {
		return fmt.Errorf("resolution root missing")
	}
	normalized, err := Resolve(ctx, root, func(_ context.Context, r Ref) (Record, error) {
		v, ok := records[r]
		if !ok {
			return v, fmt.Errorf("resolution missing %s", r)
		}
		return v, nil
	}, plan.OS, plan.Arch)
	if err != nil {
		return err
	}
	a, _ := json.Marshal(normalized)
	b, _ := json.Marshal(plan)
	if !bytes.Equal(a, b) {
		return fmt.Errorf("resolution differs from exact manifests")
	}
	return nil
}
