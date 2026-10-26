package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Document is an editable draft, separate from immutable installed manifests and runs.
// Text is intentionally allowed to be incomplete JSON until validation or execution.
type Document struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Document   string `json:"document"`
	CreatedAt  string `json:"created_at"`
	ModifiedAt string `json:"modified_at"`
}

func draftTable(kind string) (string, error) {
	switch kind {
	case "manifests":
		return "manifest_drafts", nil
	case "jobs":
		return "job_definitions", nil
	}
	return "", fmt.Errorf("draft kind must be manifests or jobs")
}

func (s *Store) Documents(ctx context.Context, kind string) ([]Document, error) {
	table, err := draftTable(kind)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, "SELECT id,name,document,created_at,modified_at FROM "+table+" ORDER BY modified_at DESC,id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Document{}
	for rows.Next() {
		var d Document
		if err = rows.Scan(&d.ID, &d.Name, &d.Document, &d.CreatedAt, &d.ModifiedAt); err != nil {
			return nil, err
		}
		items = append(items, d)
	}
	return items, rows.Err()
}

func (s *Store) Document(ctx context.Context, kind, id string) (Document, error) {
	table, err := draftTable(kind)
	if err != nil {
		return Document{}, err
	}
	var d Document
	err = s.DB.QueryRowContext(ctx, "SELECT id,name,document,created_at,modified_at FROM "+table+" WHERE id=?", id).Scan(&d.ID, &d.Name, &d.Document, &d.CreatedAt, &d.ModifiedAt)
	return d, err
}

func (s *Store) SaveDocument(ctx context.Context, kind string, d Document) (Document, error) {
	table, err := draftTable(kind)
	if err != nil {
		return d, err
	}
	if strings.TrimSpace(d.Name) == "" || len(d.Name) > 256 || len(d.Document) > 1<<20 {
		return d, fmt.Errorf("name is required (max 256 characters); document limit is 1 MiB")
	}
	stamp := now()
	if d.ID == "" {
		d.ID = uuid.NewString()
		_, err = s.DB.ExecContext(ctx, "INSERT INTO "+table+"(id,name,document,created_at,modified_at) VALUES(?,?,?,?,?)", d.ID, d.Name, d.Document, stamp, stamp)
	} else {
		var result sql.Result
		result, err = s.DB.ExecContext(ctx, "UPDATE "+table+" SET name=?,document=?,modified_at=? WHERE id=?", d.Name, d.Document, stamp, d.ID)
		if err == nil {
			n, _ := result.RowsAffected()
			if n == 0 {
				err = sql.ErrNoRows
			}
		}
	}
	if err != nil {
		return d, err
	}
	return s.Document(ctx, kind, d.ID)
}

func (s *Store) DeleteDocument(ctx context.Context, kind, id string) error {
	table, err := draftTable(kind)
	if err != nil {
		return err
	}
	r, err := s.DB.ExecContext(ctx, "DELETE FROM "+table+" WHERE id=?", id)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
