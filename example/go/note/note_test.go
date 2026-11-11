//go:build bazel

package note_test

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stefanpenner/bazel-mysql/example/go/note"
	"github.com/stefanpenner/bazel-mysql/tools/mysql/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStoreDoesNotSeeAnotherTest(t *testing.T) {
	s := openStore(t)
	insert(t, s, "private")
	assert.Equal(t, []note.Note{{ID: 1, Body: "private"}}, list(t, s))
}

var srv *bazel_mysql.Server

func TestMain(m *testing.M) {
	var err error
	srv, err = bazel_mysql.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	srv.Close()
	os.Exit(code)
}

func TestStore(t *testing.T) {
	s := openStore(t)

	assert.Empty(t, list(t, s))

	_, err := s.Insert(t.Context(), "")
	assert.ErrorIs(t, err, note.ErrEmpty)

	_, err = s.Get(t.Context(), 404)
	assert.ErrorIs(t, err, note.ErrNotFound)

	a := insert(t, s, "a")
	b := insert(t, s, "b")
	assert.Equal(t, note.Note{ID: a, Body: "a"}, get(t, s, a))
	assert.Equal(t, []note.Note{{ID: a, Body: "a"}, {ID: b, Body: "b"}}, list(t, s))
}

func openStore(t *testing.T) *note.Store {
	t.Helper()
	t.Parallel()
	name := dbName(t.Name())
	admin, err := sql.Open("mysql", srv.DSN(""))
	require.NoError(t, err)
	t.Cleanup(func() { admin.Close() })

	_, err = admin.Exec("CREATE DATABASE `" + name + "`")
	require.NoError(t, err)

	s, err := note.Open(t.Context(), srv.DSN(name))
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

func insert(t *testing.T, s *note.Store, body string) int64 {
	t.Helper()
	id, err := s.Insert(t.Context(), body)
	require.NoError(t, err)
	return id
}

func get(t *testing.T, s *note.Store, id int64) note.Note {
	t.Helper()
	n, err := s.Get(t.Context(), id)
	require.NoError(t, err)
	return n
}

func list(t *testing.T, s *note.Store) []note.Note {
	t.Helper()
	notes, err := s.List(t.Context())
	require.NoError(t, err)
	return notes
}

func dbName(testName string) string {
	var b strings.Builder
	b.WriteString("t_")
	for _, r := range strings.ToLower(testName) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() > 64 {
		return b.String()[:64]
	}
	return b.String()
}
