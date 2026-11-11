//go:build bazel

package bazel_mysql

import (
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDatadirCopyIsPrivate(t *testing.T) {
	template, err := locateDatadir()
	require.NoError(t, err)
	src, err := os.Stat(filepath.Join(template, "ibdata1"))
	require.NoError(t, err)
	dst, err := os.Stat(filepath.Join(srv.dataDir, "ibdata1"))
	require.NoError(t, err)
	assert.False(t, os.SameFile(src, dst))
}

var srv *Server

func TestMain(m *testing.M) {
	var err error
	srv, err = Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	srv.Close()
	os.Exit(code)
}

func TestPerformanceSchemaIsOff(t *testing.T) {
	db, err := sql.Open("mysql", srv.DSN(""))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	var name, value string
	err = db.QueryRow("SHOW VARIABLES LIKE 'performance_schema'").Scan(&name, &value)
	require.NoError(t, err)
	assert.Equal(t, "OFF", value)
}

func TestServerUsesDefaultsFile(t *testing.T) {
	path, err := locateDefaultsFile()
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(srv.cmd.Args), 2)
	assert.Equal(t, "--defaults-file="+path, srv.cmd.Args[1])
}

func TestServerUsesSmallSettings(t *testing.T) {
	path, err := locateDefaultsFile()
	require.NoError(t, err)
	text, err := os.ReadFile(path)
	require.NoError(t, err)
	want, hidden, err := variablesFromDefaults(string(text))
	require.NoError(t, err)

	db, err := sql.Open("mysql", srv.DSN(""))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	for name, value := range want {
		var gotName, gotValue string
		err := db.QueryRow("SHOW VARIABLES LIKE '"+name+"'").Scan(&gotName, &gotValue)
		require.NoError(t, err, name)
		assert.Equal(t, value, gotValue, name)
	}
	for name := range hidden {
		var gotName, gotValue string
		err := db.QueryRow("SHOW VARIABLES LIKE '"+name+"'").Scan(&gotName, &gotValue)
		require.ErrorIs(t, err, sql.ErrNoRows, name)
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

func TestVariablesFromDefaults(t *testing.T) {
	got, hidden, err := variablesFromDefaults("# c\n[mysqld]\nskip_log_bin\nmysqlx=OFF\n")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"log_bin": "OFF"}, got)
	assert.Equal(t, map[string]string{"mysqlx": "OFF"}, hidden)

	_, _, err = variablesFromDefaults("skip_log_bin\nskip_log_bin\n")
	require.Error(t, err)
	_, _, err = variablesFromDefaults("skip_log_bin=ON\n")
	require.Error(t, err)
	_, _, err = variablesFromDefaults("not a name=1\n")
	require.Error(t, err)
}

func TestPathFromManifestFindsDirectory(t *testing.T) {
	manifest := "repo/tools/mysql/datadir/mysql/row /tmp/x/mysql/row\n"
	got, err := pathFromManifest("repo/tools/mysql/datadir", manifest)
	require.NoError(t, err)
	assert.Equal(t, "/tmp/x", got)
}
