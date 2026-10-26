package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"unicode"

	"github.com/ariym/mf-v2/internal/hub"
	"github.com/charmbracelet/x/ansi"
	"github.com/spf13/cobra"
)

func jsonOutput(cmd *cobra.Command) bool {
	asJSON, _ := cmd.Flags().GetBool("json")
	return asJSON
}

// printOutput keeps the JSON representation intact for scripts while presenting
// collections as tables and individual records as labeled, nested lists.
func printOutput(cmd *cobra.Command, value any) error {
	if jsonOutput(cmd) {
		e := json.NewEncoder(cmd.OutOrStdout())
		e.SetIndent("", "  ")
		return e.Encode(value)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var decoded any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err = d.Decode(&decoded); err != nil {
		return err
	}
	var out bytes.Buffer
	if cmd.Name() == "list" {
		items := decoded
		page, paginated := decoded.(map[string]any)
		if paginated {
			items = page["items"]
		}
		rows, _ := items.([]any)
		printOutputTable(&out, rows, listColumns(cmd))
		if paginated {
			fmt.Fprintf(&out, "\nPage: offset %v · %d items (limit %v)\n", page["offset"], len(rows), page["limit"])
		}
	} else {
		printOutputDetails(&out, decoded, "")
	}
	_, err = io.Copy(cmd.OutOrStdout(), &out)
	return err
}

type outputColumn struct {
	label string
	field string
}

func listColumns(cmd *cobra.Command) []outputColumn {
	switch cmd.Parent().Name() {
	case "jobs":
		return []outputColumn{{"ID", "id"}, {"Flow", "flow"}, {"Version", "flow_version"}, {"State", "state"}, {"Paths", "paths"}, {"Created At", "created_at"}}
	case "segments":
		return []outputColumn{{"ID", "id"}, {"Media ID", "media_file_id"}, {"Start", "start"}, {"End", "end"}, {"Tags", "tags"}}
	case "tags":
		return []outputColumn{{"ID", "id"}, {"Tag Type", "tag_type_key"}, {"Version", "tag_type_version"}, {"Data Type", "data_type"}, {"Value", "value"}, {"Links", "links"}}
	case "tagtypes":
		return []outputColumn{{"ID", "id"}, {"Key", "definition.key"}, {"Version", "definition.version"}, {"Data Type", "definition.data_type"}, {"Source", "source"}}
	case "flows", "programs":
		return []outputColumn{{"Key", "key"}, {"Version", "version"}, {"OS", "os"}, {"Arch", "arch"}, {"Source", "source"}, {"Executable", "executable"}}
	}
	return nil
}

func printOutputTable(out *bytes.Buffer, rows []any, columns []outputColumn) {
	if len(rows) == 0 {
		fmt.Fprintln(out, "No items found.")
		return
	}
	if len(columns) == 0 {
		printOutputDetails(out, rows, "")
		return
	}
	w := tabwriter.NewWriter(out, 0, 4, 3, ' ', 0)
	headers := make([]string, len(columns))
	for i, column := range columns {
		headers[i] = strings.ToUpper(column.label)
	}
	fmt.Fprintln(w, strings.Join(headers, "\t"))
	for _, row := range rows {
		cells := make([]string, len(columns))
		for i, column := range columns {
			value := row
			for _, key := range strings.Split(column.field, ".") {
				object, _ := value.(map[string]any)
				value = object[key]
			}
			cells[i] = ansi.Truncate(outputSummary(value), 60, "…")
		}
		fmt.Fprintln(w, strings.Join(cells, "\t"))
	}
	_ = w.Flush() // bytes.Buffer writes cannot fail.
}

// Details never truncate values. Nested objects and arrays retain their
// structure without requiring readers to interpret JSON punctuation.
func printOutputDetails(out *bytes.Buffer, value any, indent string) {
	switch v := value.(type) {
	case map[string]any:
		if len(v) == 0 {
			fmt.Fprintln(out, indent+"None")
			return
		}
		w := tabwriter.NewWriter(out, 0, 4, 3, ' ', 0)
		keys := outputKeys(v)
		for _, key := range keys {
			if !outputNested(v[key]) {
				fmt.Fprintf(w, "%s%s:\t%s\n", indent, outputLabel(key), outputSummary(v[key]))
			}
		}
		_ = w.Flush()
		for _, key := range keys {
			if outputNested(v[key]) {
				fmt.Fprintf(out, "%s%s:\n", indent, outputLabel(key))
				printOutputDetails(out, v[key], indent+"  ")
			}
		}
	case []any:
		if len(v) == 0 {
			fmt.Fprintln(out, indent+"None")
		}
		for i, item := range v {
			if outputNested(item) {
				fmt.Fprintf(out, "%s%d.\n", indent, i+1)
				printOutputDetails(out, item, indent+"  ")
			} else {
				fmt.Fprintf(out, "%s- %s\n", indent, outputSummary(item))
			}
		}
	default:
		fmt.Fprintln(out, indent+outputSummary(value))
	}
}

func outputNested(value any) bool {
	switch value.(type) {
	case map[string]any, []any:
		return true
	}
	return false
}

func outputSummary(value any) string {
	switch v := value.(type) {
	case nil:
		return "—"
	case string:
		if v == "" {
			return "—"
		}
		return hub.DisplayText(v)
	case map[string]any:
		parts := make([]string, 0, len(v))
		for _, key := range outputKeys(v) {
			parts = append(parts, outputLabel(key)+": "+outputSummary(v[key]))
		}
		if len(parts) == 0 {
			return "None"
		}
		return strings.Join(parts, "; ")
	case []any:
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = outputSummary(item)
		}
		if len(parts) == 0 {
			return "None"
		}
		return strings.Join(parts, ", ")
	default:
		return fmt.Sprint(value)
	}
}

func outputLabel(key string) string {
	words := strings.Split(hub.DisplayText(key), "_")
	for i, word := range words {
		switch word {
		case "id", "ids", "os", "url", "json", "sha256":
			words[i] = strings.ToUpper(word)
		default:
			runes := []rune(word)
			if len(runes) > 0 {
				runes[0] = unicode.ToUpper(runes[0])
			}
			words[i] = string(runes)
		}
	}
	return strings.Join(words, " ")
}

func outputKeys(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	// Put record identity and status first, and order other fields consistently.
	priority := map[string]int{"id": 1, "key": 2, "identifier": 3, "version": 4, "flow": 5, "flow_version": 6, "state": 7}
	sort.Slice(keys, func(i, j int) bool {
		a, b := priority[keys[i]], priority[keys[j]]
		if a == 0 {
			a = len(priority) + 1
		}
		if b == 0 {
			b = len(priority) + 1
		}
		if a != b {
			return a < b
		}
		return keys[i] < keys[j]
	})
	return keys
}
