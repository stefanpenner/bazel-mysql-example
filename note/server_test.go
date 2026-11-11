//go:build bazel

package note_test

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/stefanpenner/bazel-mysql/test/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// server is one MySQL process for this test binary.
// The socket path is short. macOS rejects a unix socket path longer than 104 bytes.
// TEST_TMPDIR is longer than that.
type server struct {
	cmd     *exec.Cmd
	log     *os.File
	sock    string
	sockDir string
	dataDir string
	exited  chan struct{}
	exitErr error
	// owned is false when this process joined a shared mysqld.
	// TestMain calls close only when owned is true.
	owned bool
	// shareHold keeps the shared clients.lock open.
	// The os.File finalizer closes a file that no variable owns, and the lock drops.
	shareHold     *os.File
	mysqldPid     int
	supervisorPid int
	runDir        string
}

func startServer() (*server, error) {
	mysqld, err := locateMysqld()
	if err != nil {
		return nil, err
	}
	base := filepath.Dir(filepath.Dir(mysqld))
	dataRoot := os.Getenv("TEST_TMPDIR")
	if dataRoot == "" {
		return nil, errors.New("TEST_TMPDIR is empty")
	}
	dataDir := filepath.Join(dataRoot, "data")
	template, err := locateDatadir()
	if err != nil {
		return nil, err
	}
	// The template is shared. The copy is a new file, so a test cannot write the template.
	if err := mysql.CopyDatadir(template, dataDir); err != nil {
		os.RemoveAll(dataDir)
		return nil, err
	}
	sockDir, err := os.MkdirTemp("/tmp", "bzmysql")
	if err != nil {
		return nil, err
	}
	sock := filepath.Join(sockDir, "s.sock")
	pidFile := filepath.Join(dataRoot, "mysql.pid")
	logPath := filepath.Join(dataRoot, "mysql.err")
	logFile, err := os.Create(logPath)
	if err != nil {
		os.RemoveAll(sockDir)
		return nil, err
	}

	env := serverEnv(base)
	runArgs := mysqldArgs(base, dataDir, sock, pidFile, logPath, dataRoot)
	cmd := exec.Command(mysqld, runArgs...)
	cmd.Env = env
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		os.RemoveAll(sockDir)
		return nil, err
	}
	s := &server{
		cmd:     cmd,
		log:     logFile,
		sock:    sock,
		sockDir: sockDir,
		dataDir: dataDir,
		exited:  make(chan struct{}),
		owned:   true,
	}
	go func() {
		s.exitErr = cmd.Wait()
		close(s.exited)
	}()
	if err := s.waitReady(); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

func (s *server) dsn(db string) string {
	return rootSocketDSN(s.sock, db)
}

// stableFlags is the server configuration that both the private server and the shared server use.
// The mysql hash covers these flags. A flag change starts a new shared server.
func stableFlags() []string {
	return []string{
		"--skip-networking",
		"--mysqlx=OFF",
		"--skip-log-bin",
		"--skip-performance-schema",
		// The default buffer pool is 128 MiB.
		// These tests read a few rows.
		// A 16 MiB pool took a median of 286.8 ms for 2000 autocommit inserts, so this pool is 32 MiB.
		// On this 48 GiB Mac the idle process used 246.2 MiB versus 254.3 MiB.
		// Those inserts took a median of 83.2 ms versus 161.6 ms.
		"--innodb-buffer-pool-size=33554432",
		// The default log buffer is 64 MiB.
		// An 8 MiB buffer holds these transactions.
		// On this 48 GiB Mac the idle process used 198.3 MiB versus 254.3 MiB.
		"--innodb-log-buffer-size=8388608",
		// The default redo log is 100 MiB.
		// Initialize stores an 8 MiB log, and the template is 97.6 MiB versus 189.6 MiB.
		// This flag matches that template.
		"--innodb-redo-log-capacity=8388608",
		// The default flushes every commit to disk.
		// Value 0 writes and flushes the redo log about once a second.
		// The running MySQL server still returns the committed row.
		// A crash can lose the last second. These tests do not reopen the data directory after a crash.
		// 2000 autocommit inserts took a median of 6.7 ms versus 29.8 ms.
		"--innodb-flush-log-at-trx-commit=0",
		// fsync still asks the disk to store data pages at a checkpoint.
		// nosync skips that flush. A crash can tear a page.
		// 2000 autocommit inserts took a median of 26.5 ms versus 29.8 ms.
		// With value 0, the pair took 7.1 ms versus 6.7 ms.
		"--innodb-flush-method=nosync",
		// Doublewrite stores each page twice so a crash cannot tear a page.
		// These tests do not reopen the data directory after a crash.
		// Initialize omits the 16 MiB doublewrite files.
		// On this 48 GiB Mac the idle process used 196.3 MiB versus 254.3 MiB.
		// 2000 autocommit inserts took a median of 86.6 ms versus 161.6 ms.
		"--skip-innodb-doublewrite",
		// MySQL 8.4 starts the event scheduler.
		// These tests create no events.
		// One run showed 34 threads versus 35.
		// Resident memory was 253.8 MiB. The baseline spread was 253.2 MiB to 259.2 MiB.
		"--event-scheduler=OFF",
	}
}

func mysqldArgs(base, dataDir, sock, pidFile, logPath, secureRoot string) []string {
	args := []string{
		"--no-defaults",
		"--basedir=" + base,
		"--datadir=" + dataDir,
	}
	if plugin := pluginDir(base); plugin != "" {
		args = append(args, "--plugin-dir="+plugin)
	}
	if os.Geteuid() == 0 {
		args = append(args, "--user=root")
	}
	args = append(args,
		"--socket="+sock,
		"--pid-file="+pidFile,
	)
	args = append(args, stableFlags()...)
	args = append(args,
		"--secure-file-priv="+secureRoot,
		"--log-error="+logPath,
	)
	return args
}

func (s *server) waitReady() error {
	deadline := time.Now().Add(60 * time.Second)
	dsn := s.dsn("")
	for {
		select {
		case <-s.exited:
			return fmt.Errorf("mysqld exited: %w\n%s", s.exitErr, s.logTail())
		default:
		}
		db, err := sql.Open("mysql", dsn)
		if err == nil {
			err = db.Ping()
			db.Close()
			if err == nil {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("mysqld did not accept a connection\n%s", s.logTail())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (s *server) close() {
	s.stopProcess()
	if s.log != nil {
		s.log.Close()
		s.log = nil
	}
	if s.sockDir != "" {
		os.RemoveAll(s.sockDir)
	}
}

func (s *server) stopProcess() {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return
	}
	_ = s.cmd.Process.Signal(syscall.SIGTERM)
	if s.exited == nil {
		_ = s.cmd.Process.Kill()
		return
	}
	select {
	case <-s.exited:
	case <-time.After(10 * time.Second):
		_ = s.cmd.Process.Kill()
		<-s.exited
	}
}

func (s *server) logTail() string {
	b, err := os.ReadFile(s.log.Name())
	if err != nil {
		return ""
	}
	if len(b) > 4000 {
		b = b[len(b)-4000:]
	}
	return string(b)
}

func locateMysqld() (string, error) {
	markerRel := os.Getenv("MYSQLD")
	if markerRel == "" {
		return "", errors.New("MYSQLD is empty. MODULE.bazel must provide the mysql server.")
	}
	marker, err := resolveRunfile(markerRel)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(marker)
	if err != nil {
		return "", err
	}
	rel := strings.TrimSpace(string(b))
	if rel == "" {
		return "", errors.New("mysql toolchain marker is empty")
	}
	return resolveRunfile(rel)
}

func resolveRunfile(rel string) (string, error) {
	if dir := os.Getenv("RUNFILES_DIR"); dir != "" {
		p := filepath.Join(dir, rel)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	manifest := os.Getenv("RUNFILES_MANIFEST_FILE")
	if manifest == "" {
		return "", fmt.Errorf("runfiles has no %s", rel)
	}
	b, err := os.ReadFile(manifest)
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		left, right, ok := strings.Cut(line, " ")
		if ok && left == rel {
			return right, nil
		}
	}
	return "", fmt.Errorf("runfiles has no %s", rel)
}

func pluginDir(base string) string {
	for _, rel := range []string{
		filepath.Join("lib", "plugin"),
		filepath.Join("lib", "mysql", "plugin"),
	} {
		p := filepath.Join(base, rel)
		st, err := os.Stat(p)
		if err == nil && st.IsDir() {
			return p
		}
	}
	return ""
}

func locateDatadir() (string, error) {
	rel := os.Getenv("MYSQL_DATADIR")
	if rel == "" {
		return "", errors.New("MYSQL_DATADIR is empty. The test must copy the mysql data directory template.")
	}
	if dir := os.Getenv("RUNFILES_DIR"); dir != "" {
		p := filepath.Join(dir, rel)
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return p, nil
		}
	}
	manifest := os.Getenv("RUNFILES_MANIFEST_FILE")
	if manifest == "" {
		return "", fmt.Errorf("runfiles has no %s", rel)
	}
	b, err := os.ReadFile(manifest)
	if err != nil {
		return "", err
	}
	return pathFromManifest(rel, string(b))
}

func pathFromManifest(rel, manifest string) (string, error) {
	prefix := rel + "/"
	for line := range strings.SplitSeq(manifest, "\n") {
		left, right, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		if left == rel {
			return right, nil
		}
		rest, ok := strings.CutPrefix(left, prefix)
		if !ok || !strings.HasSuffix(right, rest) {
			continue
		}
		return strings.TrimRight(right[:len(right)-len(rest)], `/\`), nil
	}
	return "", fmt.Errorf("runfiles has no %s", rel)
}

func TestPathFromManifestFindsDirectory(t *testing.T) {
	manifest := "repo/tools/mysql/datadir/mysql/row /tmp/x/mysql/row\n"
	got, err := pathFromManifest("repo/tools/mysql/datadir", manifest)
	require.NoError(t, err)
	assert.Equal(t, "/tmp/x", got)
}

func serverEnv(base string) []string {
	lib := strings.Join([]string{
		filepath.Join(base, "bin"),
		filepath.Join(base, "lib"),
	}, ":")
	bin := filepath.Join(base, "bin")
	out := make([]string, 0, len(os.Environ())+2)
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "PATH=") {
			out = append(out, "PATH="+bin+":"+strings.TrimPrefix(e, "PATH="))
			continue
		}
		if strings.HasPrefix(e, "DYLD_LIBRARY_PATH=") || strings.HasPrefix(e, "LD_LIBRARY_PATH=") {
			continue
		}
		out = append(out, e)
	}
	if runtime.GOOS == "darwin" {
		out = append(out, "DYLD_LIBRARY_PATH="+lib)
		return out
	}
	out = append(out, "LD_LIBRARY_PATH="+lib)
	return out
}
