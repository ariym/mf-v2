package browser

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/ariym/mf-v2/internal/store"
)

type Point struct {
	Start, End float64
	Value      any
	TagID      int64
}
type Track struct {
	Name, DataType string
	Points         []Point
}

func Tracks(tags []store.Tag, segments []store.Segment, history bool) []Track {
	ranges := map[int64]store.Segment{}
	for _, s := range segments {
		ranges[s.ID] = s
	}
	latest := map[string]string{}
	latestJob := map[string]string{}
	for _, t := range tags {
		if t.SourceJobID != nil && t.CreatedAt > latest[t.Key] {
			latest[t.Key] = t.CreatedAt
			latestJob[t.Key] = *t.SourceJobID
		}
	}
	index := map[string]int{}
	out := []Track{}
	appendPoint := func(name, kind string, p Point) {
		n, ok := index[name]
		if !ok {
			n = len(out)
			index[name] = n
			out = append(out, Track{Name: name, DataType: kind})
		}
		out[n].Points = append(out[n].Points, p)
	}
	for _, t := range tags {
		if !history && t.SourceJobID != nil && *t.SourceJobID != latestJob[t.Key] {
			continue
		}
		var value any
		_ = json.Unmarshal(t.Value, &value)
		projected := false
		for _, link := range t.Links {
			if _, ok := ranges[link.SegmentID]; ok && link.ValuePointer != "" {
				projected = true
			}
		}
		for _, link := range t.Links {
			s, ok := ranges[link.SegmentID]
			if !ok || (projected && link.ValuePointer == "") {
				continue
			}
			a, _ := store.ParseTimecode(s.Start)
			b, _ := store.ParseTimecode(s.End)
			v, ok := store.JSONPointer(value, link.ValuePointer)
			if !ok {
				continue
			}
			name := fmt.Sprintf("%s@%d", t.Key, t.Version)
			if history && t.SourceJobID != nil {
				name += " job " + (*t.SourceJobID)[:min(8, len(*t.SourceJobID))]
			}
			p := Point{float64(a) / 1e6, float64(b) / 1e6, v, t.ID}
			if t.DataType == "tag_set" && link.ValuePointer == "" {
				if children, ok := v.(map[string]any); ok {
					keys := []string{}
					for key := range children {
						keys = append(keys, key)
					}
					sort.Strings(keys)
					for _, key := range keys {
						child := p
						child.Value = children[key]
						appendPoint(name+"/"+key, valueType(child.Value), child)
					}
					continue
				}
			}
			kind := t.DataType
			if t.DataType == "tag_set" && link.ValuePointer != "" {
				parts := strings.Split(link.ValuePointer, "/")
				if len(parts) > 1 {
					name += "/" + parts[1]
				}
				kind = "json"
			}
			appendPoint(name, kind, p)
		}
	}
	return out
}
func valueType(value any) string {
	switch value.(type) {
	case float64:
		return "number"
	case bool:
		return "boolean"
	case string:
		return "string"
	}
	return "json"
}
func scalar(v any) any {
	if object, ok := v.(map[string]any); ok {
		for _, key := range []string{"confidence", "score", "probability", "value", "speaker", "text", "label"} {
			if v, ok := object[key]; ok {
				switch v.(type) {
				case float64, bool, string:
					return v
				}
			}
		}
		return "structured"
	}
	return v
}

// Plot uses a reversed time axis: the rightmost cell is the window start.
func Plot(track Track, start, span float64, width int) string {
	if width < 4 {
		return ""
	}
	if span <= 0 {
		span = 1
	}
	cells := make([][]any, width)
	for _, p := range track.Points {
		if p.End <= start || p.Start >= start+span {
			continue
		}
		a := max(0, int(math.Floor((p.Start-start)/span*float64(width))))
		b := min(width-1, int(math.Ceil((p.End-start)/span*float64(width)))-1)
		for col := a; col <= b; col++ {
			cells[width-1-col] = append(cells[width-1-col], scalar(p.Value))
		}
	}
	numeric := true
	minV, maxV := math.Inf(1), math.Inf(-1)
	for _, values := range cells {
		for _, value := range values {
			n, ok := value.(float64)
			if !ok {
				numeric = false
				continue
			}
			minV = math.Min(minV, n)
			maxV = math.Max(maxV, n)
		}
	}
	if numeric && !math.IsInf(minV, 0) {
		levels := []rune("▁▂▃▄▅▆▇█")
		line := make([]rune, width)
		for i, values := range cells {
			line[i] = '·'
			if len(values) == 0 {
				continue
			}
			total := 0.0
			for _, v := range values {
				total += v.(float64)
			}
			fraction := 0.5
			if maxV > minV {
				fraction = (total/float64(len(values)) - minV) / (maxV - minV)
			}
			line[i] = levels[min(7, max(0, int(math.Round(fraction*7))))]
		}
		return fmt.Sprintf("%s\nvalue %.4g … %.4g", string(line), minV, maxV)
	}
	labels := map[string]rune{}
	counts := map[string]int{}
	alphabet := []rune("123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz")
	for _, p := range track.Points {
		if p.End <= start || p.Start >= start+span {
			continue
		}
		v := scalar(p.Value)
		text := fmt.Sprint(v)
		if valueType(v) == "json" {
			text = "structured"
		}
		counts[text]++
	}
	keys := []string{}
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for i, key := range keys {
		labels[key] = alphabet[i%len(alphabet)]
	}
	line := make([]rune, width)
	for i, values := range cells {
		line[i] = '·'
		if len(values) == 0 {
			continue
		}
		v := values[len(values)-1]
		if b, ok := v.(bool); ok {
			if b {
				line[i] = '█'
			} else {
				line[i] = '░'
			}
		} else {
			label := fmt.Sprint(v)
			if valueType(v) == "json" {
				label = "structured"
			}
			line[i] = labels[label]
		}
	}
	legend := []string{}
	for _, key := range keys {
		glyph := string(labels[key])
		if key == "true" {
			glyph = "█"
		}
		if key == "false" {
			glyph = "░"
		}
		label := []rune(safe(key))
		if len(label) > 40 {
			label = append(label[:39], '…')
		}
		legend = append(legend, fmt.Sprintf("%s %s (%d)", glyph, string(label), counts[key]))
	}
	return string(line) + "\n" + strings.Join(legend, " · ")
}
