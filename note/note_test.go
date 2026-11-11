//go:build bazel

package note_test

import (
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stefanpenner/bazel-mysql/note"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStoreDoesNotSeeAnotherTest(t *testing.T) {
	s := openStore(t)
	insert(t, s, "private")
	assert.Equal(t, []note.Note{{ID: 1, Body: "private"}}, list(t, s))
}

func TestDatadirCopyIsPrivate(t *testing.T) {
	template, err := locateDatadir()
	require.NoError(t, err)
	src, err := os.Stat(filepath.Join(template, "ibdata1"))
	require.NoError(t, err)
	dst, err := os.Stat(filepath.Join(srv.dataDir, "ibdata1"))
	require.NoError(t, err)
	assert.False(t, os.SameFile(src, dst))
}

var srv *server

func TestMain(m *testing.M) {
	switch os.Getenv("MYSQL_ROLE") {
	case "supervisor":
		os.Exit(runSupervisor())
	case "watch":
		os.Exit(runWatch())
	case "hold":
		os.Exit(runHold())
	case "trystop":
		os.Exit(runTryStop())
	case "idleprobe":
		os.Exit(runIdleProbe())
	case "":
	default:
		fmt.Fprintf(os.Stderr, "unknown MYSQL_ROLE %q\n", os.Getenv("MYSQL_ROLE"))
		os.Exit(2)
	}
	var err error
	if os.Getenv("MYSQL_SHARE") == "1" {
		srv, err = attachShared()
	} else {
		srv, err = startServer()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	if srv.owned {
		srv.close()
	}
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

func TestPerformanceSchemaIsOff(t *testing.T) {
	db, err := sql.Open("mysql", srv.dsn(""))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	var name, value string
	err = db.QueryRow("SHOW VARIABLES LIKE 'performance_schema'").Scan(&name, &value)
	require.NoError(t, err)
	assert.Equal(t, "OFF", value)
}

func TestServerUsesSmallSettings(t *testing.T) {
	db, err := sql.Open("mysql", srv.dsn(""))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	want := map[string]string{
		"innodb_buffer_pool_size":        "33554432",
		"innodb_log_buffer_size":         "8388608",
		"innodb_redo_log_capacity":       "8388608",
		"innodb_flush_log_at_trx_commit": "0",
		"innodb_flush_method":            "nosync",
		"innodb_doublewrite":             "OFF",
		"event_scheduler":                "OFF",
	}
	for name, value := range want {
		var gotName, gotValue string
		err := db.QueryRow("SHOW VARIABLES LIKE '"+name+"'").Scan(&gotName, &gotValue)
		require.NoError(t, err)
		assert.Equal(t, value, gotValue, name)
	}
}

func TestDataDirectoryTemplateIsSmall(t *testing.T) {
	template, err := locateDatadir()
	require.NoError(t, err)
	var redo int64
	err = filepath.WalkDir(template, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), ".dblwr") {
			t.Errorf("doublewrite file %s is in the template", d.Name())
		}
		if strings.Contains(path, "#innodb_redo") {
			// Runfiles store the redo files as symlinks. Stat follows the link.
			info, err := os.Stat(path)
			if err != nil {
				return err
			}
			redo += info.Size()
		}
		return nil
	})
	require.NoError(t, err)
	assert.Less(t, redo, int64(20<<20))
}

func openStore(t *testing.T) *note.Store {
	t.Helper()
	t.Parallel()
	if !srv.owned {
		return openSharedStore(t)
	}
	return openPrivateStore(t)
}

func openPrivateStore(t *testing.T) *note.Store {
	t.Helper()
	name := dbName(t.Name())
	admin, err := sql.Open("mysql", srv.dsn(""))
	require.NoError(t, err)
	t.Cleanup(func() { admin.Close() })

	_, err = admin.Exec("CREATE DATABASE `" + name + "`")
	require.NoError(t, err)

	s, err := note.Open(t.Context(), srv.dsn(name))
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

func openSharedStore(t *testing.T) *note.Store {
	t.Helper()
	admin, err := sql.Open("mysql", srv.dsn(""))
	require.NoError(t, err)
	t.Cleanup(func() { admin.Close() })

	db, user, pass, err := grantTenant(admin, t.Name())
	require.NoError(t, err)
	t.Cleanup(func() { dropTenant(t, admin, db, user) })

	s, err := note.Open(t.Context(), userDSN(srv.sock, user, pass, db))
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
