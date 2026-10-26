package hub

import (
	"encoding/json"
	"fmt"
)

// Execution is an opt-in runtime contract. Tags on Flow remain schema pins.
type Execution struct {
	AllowEmpty      []string          `json:"allow_empty,omitempty"`
	Preparation     map[string]string `json:"preparation,omitempty"`
	ProtocolVersion int               `json:"protocol_version"`
	Requires        []Requirement     `json:"requires,omitempty"`
	Produces        []DataSpec        `json:"produces,omitempty"`
}
type DataSpec struct {
	Kind    string `json:"kind"`
	Key     string `json:"key"`
	Version int    `json:"version"`
}
type Producer struct {
	Flow    string          `json:"flow,omitempty"`
	Builtin string          `json:"builtin,omitempty"`
	Version int             `json:"version"`
	Input   json.RawMessage `json:"input,omitempty"`
}
type Requirement struct {
	Name     string     `json:"name"`
	AnyOf    []DataSpec `json:"any_of"`
	Producer Producer   `json:"producer"`
}

func validateExecution(e *Execution, tags Versions) error {
	if e == nil {
		return nil
	}
	if e.ProtocolVersion != 2 {
		return fmt.Errorf("execution.protocol_version must be 2")
	}
	check := func(d DataSpec) error {
		if d.Kind == "tag" && ValidIdentifier(d.Key) && d.Version > 0 && tags[d.Key] == d.Version {
			return nil
		}
		if d.Kind == "audio" && d.Key == "audio" && d.Version == 1 {
			return nil
		}
		return fmt.Errorf("invalid execution data %s/%s@%d (tag types must be pinned)", d.Kind, d.Key, d.Version)
	}
	for program, profile := range e.Preparation {
		if !ValidIdentifier(program) || !ValidIdentifier(profile) {
			return fmt.Errorf("invalid preparation selection")
		}
	}
	names := map[string]bool{}
	for _, r := range e.Requires {
		if !ValidIdentifier(r.Name) || names[r.Name] || len(r.AnyOf) == 0 {
			return fmt.Errorf("requirements need unique names and nonempty any_of")
		}
		names[r.Name] = true
		for _, d := range r.AnyOf {
			if err := check(d); err != nil {
				return err
			}
		}
		p := r.Producer
		if p.Version < 1 || (p.Flow == "") == (p.Builtin == "") {
			return fmt.Errorf("requirement %s needs one exact-version producer", r.Name)
		}
		if p.Flow != "" && !ValidIdentifier(p.Flow) {
			return fmt.Errorf("invalid producer flow")
		}
		if p.Builtin != "" {
			if p.Builtin != "extract_audio" || p.Version != 1 {
				return fmt.Errorf("unsupported built-in producer %s@%d", p.Builtin, p.Version)
			}
			if len(r.AnyOf) != 1 || r.AnyOf[0] != (DataSpec{"audio", "audio", 1}) {
				return fmt.Errorf("extract_audio produces audio/audio@1")
			}
		}
		if len(p.Input) > 0 {
			var obj map[string]json.RawMessage
			if err := DecodeStrict(p.Input, &obj); err != nil || obj == nil {
				return fmt.Errorf("producer input must be an object")
			}
		}
	}
	for _, d := range e.Produces {
		if err := check(d); err != nil {
			return err
		}
	}
	for _, key := range e.AllowEmpty {
		found := false
		for _, out := range e.Produces {
			found = found || (out.Kind == "tag" && out.Key == key)
		}
		if !found {
			return fmt.Errorf("allow_empty must name a declared tag output")
		}
	}
	return nil
}

func executionDependencies(e *Execution) []Ref {
	var out []Ref
	if e != nil {
		for _, r := range e.Requires {
			if r.Producer.Flow != "" {
				out = append(out, Ref{Flows, r.Producer.Flow, r.Producer.Version})
			}
		}
	}
	return out
}

func validateProducers(r Record, dependencies []Record) error {
	f, ok := r.Value.(Flow)
	if !ok || f.Execution == nil {
		return nil
	}
	for key, profile := range f.Execution.Preparation {
		version, ok := f.Programs[key]
		if !ok {
			return E("invalid_dependencies", "preparation program %s must be a direct dependency", key)
		}
		found := false
		for _, dep := range dependencies {
			if p, ok := dep.Value.(Program); ok && p.ProgramName == key && p.Version == version {
				_, found = p.Preparation[profile]
			}
		}
		if !found {
			return E("invalid_dependencies", "program %s@%d does not declare preparation %s", key, version, profile)
		}
	}
	for _, req := range f.Execution.Requires {
		if req.Producer.Flow == "" {
			continue
		}
		valid := false
		for _, dep := range dependencies {
			producer, ok := dep.Value.(Flow)
			if !ok || producer.Key != req.Producer.Flow || producer.Version != req.Producer.Version || producer.Execution == nil {
				continue
			}
			for _, output := range producer.Execution.Produces {
				for _, wanted := range req.AnyOf {
					if output == wanted {
						valid = true
					}
				}
			}
		}
		if !valid {
			return E("invalid_dependencies", "%s producer does not declare an acceptable output for %s", r.Ref, req.Name)
		}
	}
	return nil
}
