// Package note stores a short text note in MySQL.
package note

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	_ "github.com/go-sql-driver/mysql"
)

var (
	ErrNotFound = errors.New("note not found")
	ErrEmpty    = errors.New("note body is empty")
)

// Note is one row.
type Note struct {
	ID   int64
	Body string
}

// Store reads and writes notes in one MySQL database.
type Store struct {
	db *sql.DB
}

// Open connects to MySQL and creates the notes table.
func Open(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS notes (
  id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
  body TEXT NOT NULL
)`)
	if err != nil {
		return fmt.Errorf("create notes table: %w", err)
	}
	return nil
}

func (s *Store) Insert(ctx context.Context, body string) (int64, error) {
	if body == "" {
		return 0, ErrEmpty
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO notes (body) VALUES (?)`, body)
	if err != nil {
		return 0, fmt.Errorf("insert note: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("insert note id: %w", err)
	}
	return id, nil
}

func (s *Store) Get(ctx context.Context, id int64) (Note, error) {
	var n Note
	err := s.db.QueryRowContext(ctx, `SELECT id, body FROM notes WHERE id = ?`, id).Scan(&n.ID, &n.Body)
	if errors.Is(err, sql.ErrNoRows) {
		return Note{}, ErrNotFound
	}
	if err != nil {
		return Note{}, fmt.Errorf("get note: %w", err)
	}
	return n, nil
}

func (s *Store) List(ctx context.Context) ([]Note, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, body FROM notes ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list notes: %w", err)
	}
	defer rows.Close()
	notes := []Note{}
	for rows.Next() {
		var n Note
		if err := rows.Scan(&n.ID, &n.Body); err != nil {
			return nil, fmt.Errorf("list notes: %w", err)
		}
		notes = append(notes, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list notes: %w", err)
	}
	return notes, nil
}
