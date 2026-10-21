package hub

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

// Entry is the display metadata of a validated catalog definition. Ref always
// pins the listed version, even if newer versions appear while browsing.
type Entry struct {
	Ref                Ref
	Title, Description string
}

func (e Entry) Name() string  { return fmt.Sprintf("%s@%d", e.Ref.Identifier, e.Ref.Version) }
func (e Entry) Label() string { return LocalKind(e.Ref.Kind) + "/" + e.Name() }

func Entries(kind string, page Page) ([]Entry, error) {
	entries := make([]Entry, 0, len(page.Items))
	for _, raw := range page.Items {
		record, err := Decode(WireKind(kind), raw)
		if err != nil {
			return nil, err
		}
		entry := Entry{Ref: record.Ref}
		switch value := record.Value.(type) {
		case TagType:
			entry.Title, entry.Description = value.Title, value.Desc
			if entry.Title == "" {
				entry.Title = value.DataType
			} else {
				entry.Title += " · " + value.DataType
			}
		case Flow:
			entry.Title, entry.Description = value.Title, value.Desc
		case Program:
			entry.Description = value.Desc
		}
		entry.Title, entry.Description = DisplayText(entry.Title), DisplayText(entry.Description)
		entries = append(entries, entry)
	}
	return entries, nil
}

// DisplayText keeps remote metadata on a single terminal line without escapes.
func DisplayText(value string) string {
	return strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, ansi.Strip(value))), " ")
}
