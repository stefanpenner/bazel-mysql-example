// Package bazel_mysql starts one MySQL process for a test binary.
package bazel_mysql

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
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/stefanpenner/bazel-mysql/tools/mysql/copy"
)

// Server is one MySQL process for this test binary.
// The socket path is short. macOS rejects a unix socket path longer than 104 bytes.
// TEST_TMPDIR is longer than that.
type Server struct {
	cmd     *exec.Cmd
	log     *os.File
	sock    string
	sockDir string
	dataDir string
	exited  chan struct{}
	exitErr error
}

func Start() (*Server, error) {
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
	if err := copy.CopyDatadir(template, dataDir); err != nil {
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

	defaults, err := locateDefaultsFile()
	if err != nil {
		logFile.Close()
		os.RemoveAll(sockDir)
		return nil, err
	}
	// --defaults-file is the first argument.
	// MySQL reads that file and no host option file.
	// The file is the only copy of the tuning flags.
	args := []string{
		"--defaults-file=" + defaults,
		// basedir is the MySQL install that contains this mysqld.
		// The server loads its share files and plugins from that install.
		// Impact: the server does not use a host MySQL install.
		"--basedir=" + base,
		// datadir is the private copy under TEST_TMPDIR.
		// The shared template stays read-only.
		// Impact: a test write stays in this copy.
		"--datadir=" + dataDir,
	}
	if plugin := pluginDir(base); plugin != "" {
		// plugin-dir is the plugin directory inside basedir.
		// MySQL loads server plugins only from that directory.
		// Impact: the server does not load a host plugin.
		args = append(args, "--plugin-dir="+plugin)
	}
	if os.Geteuid() == 0 {
		// MySQL refuses to run as root without --user.
		// Some builds run the test as uid 0.
		// Impact: the server starts when the uid is 0.
		args = append(args, "--user=root")
	}
	env := serverEnv(base)

	runArgs := append(append([]string{}, args...),
		// The socket is under /tmp.
		// macOS rejects a unix socket path longer than 104 bytes, and TEST_TMPDIR is longer.
		// Impact: the client connects on this socket.
		"--socket="+sock,
		// The pid file records the server pid in the test temp directory.
		// Impact: the pid is outside the data directory template.
		"--pid-file="+pidFile,
		// secure-file-priv limits file reads and writes to this directory.
		// The directory is the test temp directory.
		// Impact: the server cannot read host files outside that directory.
		"--secure-file-priv="+dataRoot,
		// log-error is the server error log in the test temp directory.
		// The test prints the tail when the server fails to start.
		// Impact: a failed start leaves the reason next to the test.
		"--log-error="+logPath,
	)
	cmd := exec.Command(mysqld, runArgs...)
	cmd.Env = env
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		os.RemoveAll(sockDir)
		return nil, err
	}
	s := &Server{
		cmd:     cmd,
		log:     logFile,
		sock:    sock,
		sockDir: sockDir,
		dataDir: dataDir,
		exited:  make(chan struct{}),
	}
	go func() {
		s.exitErr = cmd.Wait()
		close(s.exited)
	}()
	if err := s.waitReady(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Server) DSN(db string) string {
	return fmt.Sprintf("root@unix(%s)/%s?parseTime=true&charset=utf8mb4&timeout=5s", s.sock, db)
}

func (s *Server) waitReady() error {
	deadline := time.Now().Add(60 * time.Second)
	dsn := s.DSN("")
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

func (s *Server) Close() {
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-s.exited:
		case <-time.After(10 * time.Second):
			_ = s.cmd.Process.Kill()
			<-s.exited
		}
	}
	if s.log != nil {
		s.log.Close()
	}
	os.RemoveAll(s.sockDir)
}

func (s *Server) logTail() string {
	b, err := os.ReadFile(s.log.Name())
	if err != nil {
		return ""
	}
	if len(b) > 4000 {
		b = b[len(b)-4000:]
	}
	return string(b)
}

func locateDefaultsFile() (string, error) {
	rel := os.Getenv("MYSQL_DEFAULTS_FILE")
	if rel == "" {
		return "", errors.New("MYSQL_DEFAULTS_FILE is empty. The test must receive tools/mysql/mysqld.cnf.")
	}
	return resolveRunfile(rel)
}

func variablesFromDefaults(text string) (map[string]string, map[string]string, error) {
	want := map[string]string{}
	hidden := map[string]string{}
	for raw := range strings.SplitSeq(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		key, value, hasValue := strings.Cut(line, "=")
		key = strings.ReplaceAll(strings.TrimSpace(key), "-", "_")
		if !validOptionName(key) {
			return nil, nil, fmt.Errorf("option name %q is not a MySQL name", key)
		}
		switch key {
		case "skip_log_bin", "skip_performance_schema", "skip_innodb_doublewrite", "skip_networking":
			if hasValue {
				return nil, nil, fmt.Errorf("option %s takes no value", key)
			}
		default:
			if !hasValue {
				return nil, nil, fmt.Errorf("option %s has no value", key)
			}
		}
		varName, varValue, shown := showVariable(key, strings.TrimSpace(value))
		if !shown {
			if _, ok := hidden[key]; ok {
				return nil, nil, fmt.Errorf("option %s is repeated", key)
			}
			hidden[key] = strings.TrimSpace(value)
			continue
		}
		if _, ok := want[varName]; ok {
			return nil, nil, fmt.Errorf("option %s is repeated", varName)
		}
		want[varName] = varValue
	}
	if len(want) == 0 {
		return nil, nil, errors.New("defaults file has no options")
	}
	return want, hidden, nil
}

func showVariable(key, value string) (string, string, bool) {
	switch key {
	case "skip_log_bin":
		return "log_bin", "OFF", true
	case "skip_performance_schema":
		return "performance_schema", "OFF", true
	case "skip_innodb_doublewrite":
		return "innodb_doublewrite", "OFF", true
	case "skip_networking":
		return "skip_networking", "ON", true
	case "mysqlx":
		// MySQL 8.4.13 has no system variable named mysqlx.
		// SHOW VARIABLES LIKE 'mysqlx' returns no row when the server is started with mysqlx=OFF.
		return "", "", false
	default:
		return key, value, true
	}
}

func validOptionName(key string) bool {
	if key == "" {
		return false
	}
	for _, r := range key {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
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
