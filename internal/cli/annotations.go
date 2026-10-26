package cli

import (
	"database/sql"
	"fmt"

	"github.com/ariym/mf-v2/internal/config"
	"github.com/ariym/mf-v2/internal/hub"
	"github.com/ariym/mf-v2/internal/store"
	"github.com/spf13/cobra"
)

func addAnnotationCommands(root *cobra.Command, c *config.Config) {
	segments := &cobra.Command{Use: "segments", Short: "Manage media ranges stored as timecodes"}
	segments.AddCommand(&cobra.Command{Use: "list [file]", Short: "List segments, optionally for a file", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		s, e := openStore(c)
		if e != nil {
			return e
		}
		defer s.Close()
		var mediaID int64
		if len(args) > 0 {
			m, e := importedFile(cmd.Context(), s, args[0], false)
			if e != nil {
				return e
			}
			mediaID = m.ID
		}
		v, e := s.Segments(cmd.Context(), mediaID)
		if e != nil {
			return e
		}
		return printOutput(cmd, v)
	}})
	segments.AddCommand(&cobra.Command{Use: "show <id>", Short: "Show a segment and its tag instance IDs", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, e := numericID(args[0])
		if e != nil {
			return e
		}
		s, e := openStore(c)
		if e != nil {
			return e
		}
		defer s.Close()
		v, e := s.Segment(cmd.Context(), id)
		if e != nil {
			return e
		}
		return printOutput(cmd, v)
	}})
	segments.AddCommand(&cobra.Command{Use: "add <file> <start> <end>", Short: "Add a segment, automatically importing the file", Args: cobra.ExactArgs(3), RunE: func(cmd *cobra.Command, args []string) error {
		s, e := openStore(c)
		if e != nil {
			return e
		}
		defer s.Close()
		m, e := importedFile(cmd.Context(), s, args[0], true)
		if e != nil {
			return e
		}
		id, e := s.AddSegment(cmd.Context(), m.ID, args[1], args[2])
		if e != nil {
			return e
		}
		fmt.Fprintln(cmd.OutOrStdout(), id)
		return nil
	}})
	segments.AddCommand(&cobra.Command{Use: "update <id> <start> <end>", Short: "Update a segment's media timecodes", Args: cobra.ExactArgs(3), RunE: func(cmd *cobra.Command, args []string) error {
		id, e := numericID(args[0])
		if e != nil {
			return e
		}
		s, e := openStore(c)
		if e != nil {
			return e
		}
		defer s.Close()
		return s.UpdateSegment(cmd.Context(), id, args[1], args[2])
	}})
	segments.AddCommand(&cobra.Command{Use: "remove <id>", Short: "Remove a segment and unreferenced instances", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, e := numericID(args[0])
		if e != nil {
			return e
		}
		s, e := openStore(c)
		if e != nil {
			return e
		}
		defer s.Close()
		return s.RemoveSegment(cmd.Context(), id)
	}})
	root.AddCommand(segments)
	tags := &cobra.Command{Use: "tags", Short: "Manage tag instances attached to segments"}
	tags.AddCommand(&cobra.Command{Use: "list [file]", Short: "List instances, optionally for a file", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		s, e := openStore(c)
		if e != nil {
			return e
		}
		defer s.Close()
		var mediaID int64
		if len(args) > 0 {
			m, e := importedFile(cmd.Context(), s, args[0], false)
			if e != nil {
				return e
			}
			mediaID = m.ID
		}
		v, e := s.Tags(cmd.Context(), mediaID)
		if e != nil {
			return e
		}
		return printOutput(cmd, v)
	}})
	tags.AddCommand(&cobra.Command{Use: "show <id>", Short: "Show a tag instance and its shared segment links", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, e := numericID(args[0])
		if e != nil {
			return e
		}
		s, e := openStore(c)
		if e != nil {
			return e
		}
		defer s.Close()
		values, e := s.Tags(cmd.Context(), 0)
		if e != nil {
			return e
		}
		for _, v := range values {
			if v.ID == id {
				return printOutput(cmd, v)
			}
		}
		return sql.ErrNoRows
	}})
	var value string
	var ids []int64
	add := &cobra.Command{Use: "add <tagtype[@version]>", Short: "Create a typed instance on one or more segments", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		ref, e := hub.ParseRef("tagtypes", args[0])
		if e != nil {
			return e
		}
		raw, e := jsonInput(value)
		if e != nil {
			return e
		}
		links := []store.TagLink{}
		for _, id := range ids {
			links = append(links, store.TagLink{SegmentID: id})
		}
		s, e := openStore(c)
		if e != nil {
			return e
		}
		defer s.Close()
		id, e := s.AddTag(cmd.Context(), ref.Identifier, ref.Version, raw, links)
		if e != nil {
			return e
		}
		fmt.Fprintln(cmd.OutOrStdout(), id)
		return nil
	}}
	add.Flags().StringVar(&value, "value", "", "JSON value or @file")
	add.Flags().Int64SliceVar(&ids, "segment", nil, "segment IDs, comma separated or repeated")
	_ = add.MarkFlagRequired("value")
	_ = add.MarkFlagRequired("segment")
	tags.AddCommand(add)
	var updatedValue string
	update := &cobra.Command{Use: "update <id>", Short: "Replace and validate a shared instance's value", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, e := numericID(args[0])
		if e != nil {
			return e
		}
		raw, e := jsonInput(updatedValue)
		if e != nil {
			return e
		}
		s, e := openStore(c)
		if e != nil {
			return e
		}
		defer s.Close()
		return s.UpdateTag(cmd.Context(), id, raw)
	}}
	update.Flags().StringVar(&updatedValue, "value", "", "JSON value or @file")
	_ = update.MarkFlagRequired("value")
	tags.AddCommand(update)
	tags.AddCommand(&cobra.Command{Use: "remove <id>", Short: "Remove an instance and all its segment links", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, e := numericID(args[0])
		if e != nil {
			return e
		}
		s, e := openStore(c)
		if e != nil {
			return e
		}
		defer s.Close()
		return s.RemoveTag(cmd.Context(), id)
	}})
	root.AddCommand(tags)
}
