//go:build bazel

package note_test

import (
	"bufio"
	"bytes"
	"database/sql"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChooseShare(t *testing.T) {
	if chooseShare("darwin", true) != shareAttach {
		t.Fatal("darwin live")
	}
	if chooseShare("darwin", false) != shareSupervise {
		t.Fatal("darwin idle")
	}
	if chooseShare("linux", true) != shareAttach {
		t.Fatal("linux live")
	}
	if chooseShare("linux", false) != sharePrivate {
		t.Fatal("linux idle")
	}
}

func TestSupervisorStartRefusesOtherOS(t *testing.T) {
	err := supervisorAllowed("linux")
	if err == nil || !strings.Contains(err.Error(), "linux") {
		t.Fatal(err)
	}
	if err := supervisorAllowed("darwin"); err != nil {
		t.Fatal(err)
	}
}

func TestStopAllowedMatchesSpec(t *testing.T) {
	if !stopAllowed(true, true) {
		t.Fatal("free grace must stop")
	}
	if stopAllowed(false, true) {
		t.Fatal("start.lock must block stop")
	}
	if stopAllowed(true, false) {
		t.Fatal("a holder must block stop")
	}
}

func TestCanonicalSocketPathIs70Bytes(t *testing.T) {
	p := filepath.Join("/tmp/bzmysql", strings.Repeat("a", 16), strings.Repeat("b", 16), strings.Repeat("c", 16), "s.sock")
	if len(p) != 70 {
		t.Fatalf("len %d", len(p))
	}
	if _, err := checkSocketPath(p); err != nil {
		t.Fatal(err)
	}
}

func TestSocketPathRejects104Bytes(t *testing.T) {
	p := "/" + strings.Repeat("a", 103)
	if len(p) != 104 {
		t.Fatal(len(p))
	}
	_, err := checkSocketPath(p)
	if err == nil || !strings.Contains(err.Error(), "104") {
		t.Fatal(err)
	}
	ok := "/" + strings.Repeat("a", 102)
	if _, err := checkSocketPath(ok); err != nil {
		t.Fatal(err)
	}
}

func TestOutputBaseFromExecroot(t *testing.T) {
	root := t.TempDir()
	start := filepath.Join(root, "execroot", "main", "tmp")
	require.NoError(t, os.MkdirAll(start, 0700))
	got, err := outputBaseFrom(start)
	require.NoError(t, err)
	assert.Equal(t, root, got)
}

func TestOutputBaseSkipsActionSandbox(t *testing.T) {
	root := t.TempDir()
	start := filepath.Join(root, "sandbox", "darwin-sandbox", "37", "execroot", "main", "tmp")
	require.NoError(t, os.MkdirAll(start, 0700))
	got, err := outputBaseFrom(start)
	require.NoError(t, err)
	assert.Equal(t, root, got)
}

func TestBazelOutputBaseSkipsActionSandbox(t *testing.T) {
	tmp := os.Getenv("TEST_TMPDIR")
	if tmp == "" {
		t.Skip("TEST_TMPDIR is empty")
	}
	base, err := outputBaseFrom(tmp)
	require.NoError(t, err)
	if strings.Contains(base, "darwin-sandbox") || strings.Contains(base, "linux-sandbox") {
		t.Fatalf("output base %s is an action sandbox", base)
	}
}

func TestOutputBaseMissing(t *testing.T) {
	_, err := outputBaseFrom("/tmp/not-bazel-share-test")
	if err == nil || !strings.Contains(err.Error(), "execroot") {
		t.Fatal(err)
	}
	_, err = outputBaseFrom("")
	if err == nil {
		t.Fatal("empty path")
	}
}

func TestShareRootRequiresOutputBase(t *testing.T) {
	t.Setenv("MYSQL_SHARE_ROOT", "")
	t.Setenv("TEST_TMPDIR", "/tmp/not-bazel-share-test")
	t.Setenv("RUNFILES_DIR", "/tmp/not-bazel-share-runfiles")
	_, err := shareRoot()
	if err == nil || !strings.Contains(err.Error(), "execroot") {
		t.Fatal(err)
	}
}

func TestShareRootRejectsFileParent(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0600))
	t.Setenv("MYSQL_SHARE_ROOT", filepath.Join(file, "root"))
	_, err := shareRoot()
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestBadCurrentIsAnError(t *testing.T) {
	root := shortRoot(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "current"), []byte("nope\n"), 0600))
	_, err := readCurrent(root)
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatal(err)
	}
}

func TestBadIdleIsAnError(t *testing.T) {
	t.Setenv("MYSQL_SHARE_IDLE", "soon")
	_, err := shareIdle()
	if err == nil || !strings.Contains(err.Error(), "MYSQL_SHARE_IDLE") {
		t.Fatal(err)
	}
	t.Setenv("MYSQL_SHARE_IDLE", "0s")
	_, err = shareIdle()
	if err == nil {
		t.Fatal("zero idle")
	}
}

func TestParseServerInfoMissingField(t *testing.T) {
	_, err := parseServerInfo("socket=/tmp/s.sock\n")
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatal(err)
	}
}

func TestSharedNamesFitMySQLLimits(t *testing.T) {
	db, user, pass, err := sharedNames(strings.Repeat("Test", 80))
	require.NoError(t, err)
	if len(db) > 64 || len(user) > 32 || len(pass) == 0 {
		t.Fatalf("db %d user %d", len(db), len(user))
	}
	db2, user2, _, err := sharedNames(strings.Repeat("Test", 80))
	require.NoError(t, err)
	if db == db2 || user == user2 {
		t.Fatal("tenant names did not change")
	}
}

func TestPrivateServerIsOwned(t *testing.T) {
	if os.Getenv("MYSQL_SHARE") == "1" {
		assert.False(t, srv.owned)
		return
	}
	assert.True(t, srv.owned)
}

func TestLiveSupervisorWithoutServerFileIsAnError(t *testing.T) {
	root := shortRoot(t)
	id := "0123456789abcdef"
	runDir := filepath.Join(root, id)
	require.NoError(t, os.MkdirAll(runDir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "current"), []byte(id+"\n"), 0600))
	hold, err := lockFile(filepath.Join(runDir, "supervisor.lock"), syscall.LOCK_EX)
	require.NoError(t, err)
	t.Cleanup(func() { unlockClose(hold) })
	_, err = runLive(root, id)
	if err == nil || !strings.Contains(err.Error(), "server.txt") {
		t.Fatal(err)
	}
}

func TestUnhealthyLiveServerIsAnError(t *testing.T) {
	root := shortRoot(t)
	id := "0123456789abcdef"
	runDir := filepath.Join(root, id)
	require.NoError(t, os.MkdirAll(runDir, 0700))
	hold, err := lockFile(filepath.Join(runDir, "supervisor.lock"), syscall.LOCK_EX)
	require.NoError(t, err)
	t.Cleanup(func() { unlockClose(hold) })
	info := serverInfo{
		sock:          filepath.Join(runDir, "missing.sock"),
		dataDir:       filepath.Join(runDir, "data"),
		mysqldPath:    "/usr/sbin/mysqld",
		mysqldPid:     os.Getpid(),
		supervisorPid: os.Getpid(),
		mysqlHash:     "0123456789abcdef",
	}
	require.NoError(t, writeServerInfo(filepath.Join(runDir, "server.txt"), info))
	_, err = runLive(root, id)
	if err == nil || !strings.Contains(err.Error(), "not healthy") {
		t.Fatal(err)
	}
}

func TestDeadSupervisorDoesNotKillUnrelatedPid(t *testing.T) {
	root := shortRoot(t)
	id := "0123456789abcdef"
	runDir := filepath.Join(root, id)
	require.NoError(t, os.MkdirAll(runDir, 0700))
	info := serverInfo{
		sock:          filepath.Join(runDir, "missing.sock"),
		dataDir:       "/tmp/not-our-datadir",
		mysqldPath:    "/usr/sbin/mysqld",
		mysqldPid:     os.Getpid(),
		supervisorPid: 1,
		mysqlHash:     "0123456789abcdef",
	}
	require.NoError(t, writeServerInfo(filepath.Join(runDir, "server.txt"), info))
	live, err := runLive(root, id)
	require.NoError(t, err)
	if live {
		t.Fatal("dead supervisor looked live")
	}
	if !processAlive(os.Getpid()) {
		t.Fatal("stop killed this test")
	}
}

func TestStopLocksWhenClientsHeld(t *testing.T) {
	root := shortRoot(t)
	id := "0123456789abcdef"
	require.NoError(t, os.MkdirAll(filepath.Join(root, id), 0700))
	hold, err := lockFile(filepath.Join(root, id, "clients.lock"), syscall.LOCK_SH)
	require.NoError(t, err)
	t.Cleanup(func() { unlockClose(hold) })
	got, err := runTryStopOnce(t, root, id, 5*time.Second)
	require.NoError(t, err)
	assert.Equal(t, stopHolders, got)
}

func TestStopLocksWhenStartHeld(t *testing.T) {
	root := shortRoot(t)
	id := "0123456789abcdef"
	require.NoError(t, os.MkdirAll(filepath.Join(root, id), 0700))
	start, err := lockFile(filepath.Join(root, "start.lock"), syscall.LOCK_EX)
	require.NoError(t, err)
	t.Cleanup(func() { unlockClose(start) })
	got, err := runTryStopOnce(t, root, id, 5*time.Second)
	require.NoError(t, err)
	assert.Equal(t, stopStart, got)
}

func TestStopLocksWhenFree(t *testing.T) {
	root := shortRoot(t)
	id := "0123456789abcdef"
	require.NoError(t, os.MkdirAll(filepath.Join(root, id), 0700))
	got, err := runTryStopOnce(t, root, id, 5*time.Second)
	require.NoError(t, err)
	assert.Equal(t, stopNow, got)
}

func TestIdleProbeStopsWhenFree(t *testing.T) {
	root := shortRoot(t)
	id := "0123456789abcdef"
	require.NoError(t, os.MkdirAll(filepath.Join(root, id), 0700))
	cmd := exec.Command(os.Args[0])
	cmd.Env = roleEnv(root, "idleprobe", id)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%v %s", err, stderr.String())
		}
	case <-time.After(8 * time.Second):
		cmd.Process.Kill()
		t.Fatal("idle probe did not stop")
	}
}

func TestUnknownRoleExits(t *testing.T) {
	cmd := exec.Command(os.Args[0])
	cmd.Env = putEnv(os.Environ(), "MYSQL_ROLE", "nope")
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 {
		t.Fatalf("exit %v", err)
	}
}

func TestMysqlHashIs16Hex(t *testing.T) {
	sum, err := mysqlHash()
	require.NoError(t, err)
	if len(sum) != 16 || !isHex(sum) {
		t.Fatalf("hash %q", sum)
	}
}

func TestTwoHoldsShareOneMysqld(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("shared supervisor is darwin only")
	}
	root := shortRoot(t)
	a := startHold(t, root)
	b := startHold(t, root)
	if a.socket != b.socket || a.mysqld != b.mysqld || a.mysqld == 0 {
		t.Fatalf("socket %s vs %s pid %d vs %d", a.socket, b.socket, a.mysqld, b.mysqld)
	}
	assertSharedTenant(t, a.socket)
	a.release()
	b.release()
	waitExit(t, a.cmd, 5*time.Second)
	waitExit(t, b.cmd, 5*time.Second)
	waitDead(t, a.mysqld, 8*time.Second)
}

func TestWatchStopsMysqldWhenSupervisorDies(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("shared supervisor is darwin only")
	}
	root := shortRoot(t)
	first := startHold(t, root)
	require.NoError(t, syscall.Kill(first.supervisor, syscall.SIGKILL))
	waitDead(t, first.mysqld, 15*time.Second)
	second := startHold(t, root)
	if second.mysqld == first.mysqld {
		t.Fatal("second hold joined a dead mysqld")
	}
}

func TestSharedUserCannotShutdown(t *testing.T) {
	if srv.owned {
		t.Skip("private server keeps the root user")
	}
	db, user, pass := mustTenant(t, srv.dsn(""))
	conn, err := sql.Open("mysql", userDSN(srv.sock, user, pass, db))
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	_, err = conn.Exec("SHUTDOWN")
	assert.Error(t, err)
}

func TestSharedUserCannotReadOtherDatabase(t *testing.T) {
	if srv.owned {
		t.Skip("private server keeps the root user")
	}
	admin, err := sql.Open("mysql", srv.dsn(""))
	require.NoError(t, err)
	t.Cleanup(func() { admin.Close() })
	db1, user1, pass1, err := grantTenant(admin, t.Name()+"/one")
	require.NoError(t, err)
	t.Cleanup(func() { dropTenant(t, admin, db1, user1) })
	db2, user2, pass2, err := grantTenant(admin, t.Name()+"/two")
	require.NoError(t, err)
	t.Cleanup(func() { dropTenant(t, admin, db2, user2) })

	a, err := sql.Open("mysql", userDSN(srv.sock, user1, pass1, db1))
	require.NoError(t, err)
	t.Cleanup(func() { a.Close() })
	_, err = a.Exec("CREATE TABLE notes (id INT)")
	require.NoError(t, err)
	_, err = a.Exec("INSERT INTO notes (id) VALUES (1)")
	require.NoError(t, err)

	b, err := sql.Open("mysql", userDSN(srv.sock, user2, pass2, db2))
	require.NoError(t, err)
	t.Cleanup(func() { b.Close() })
	_, err = b.Exec("SELECT id FROM `" + db1 + "`.notes")
	assert.Error(t, err)
}

func assertSharedTenant(t *testing.T, sock string) {
	t.Helper()
	admin, err := sql.Open("mysql", rootSocketDSN(sock, ""))
	require.NoError(t, err)
	defer admin.Close()
	var name, value string
	require.NoError(t, admin.QueryRow("SHOW VARIABLES LIKE 'performance_schema'").Scan(&name, &value))
	assert.Equal(t, "OFF", value)
	require.NoError(t, admin.QueryRow("SHOW VARIABLES LIKE 'innodb_buffer_pool_size'").Scan(&name, &value))
	assert.Equal(t, "33554432", value)

	db1, user1, pass1, err := grantTenant(admin, "hold-a")
	require.NoError(t, err)
	defer dropTenant(t, admin, db1, user1)
	db2, user2, pass2, err := grantTenant(admin, "hold-b")
	require.NoError(t, err)
	defer dropTenant(t, admin, db2, user2)

	a, err := sql.Open("mysql", userDSN(sock, user1, pass1, db1))
	require.NoError(t, err)
	defer a.Close()
	_, err = a.Exec("CREATE TABLE notes (id INT)")
	require.NoError(t, err)
	_, err = a.Exec("INSERT INTO notes (id) VALUES (1)")
	require.NoError(t, err)
	_, err = a.Exec("SHUTDOWN")
	assert.Error(t, err)

	b, err := sql.Open("mysql", userDSN(sock, user2, pass2, db2))
	require.NoError(t, err)
	defer b.Close()
	_, err = b.Exec("SELECT id FROM `" + db1 + "`.notes")
	assert.Error(t, err)
}

func mustTenant(t *testing.T, adminDSN string) (string, string, string) {
	t.Helper()
	admin, err := sql.Open("mysql", adminDSN)
	require.NoError(t, err)
	t.Cleanup(func() { admin.Close() })
	db, user, pass, err := grantTenant(admin, t.Name())
	require.NoError(t, err)
	t.Cleanup(func() { dropTenant(t, admin, db, user) })
	return db, user, pass
}

func shortRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bzshare")
	require.NoError(t, err)
	t.Cleanup(func() {
		var last error
		deadline := time.Now().Add(2 * time.Second)
		for {
			last = os.RemoveAll(dir)
			if last == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("delete %s: %v", dir, last)
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
	return dir
}

type holdProc struct {
	socket     string
	mysqld     int
	supervisor int
	cmd        *exec.Cmd
	stdin      io.WriteCloser
}

func (h *holdProc) release() {
	if h.stdin != nil {
		h.stdin.Close()
		h.stdin = nil
	}
}

func (h *holdProc) stop() {
	h.release()
	if h.supervisor > 0 {
		_ = syscall.Kill(h.supervisor, syscall.SIGKILL)
	}
	if h.cmd != nil && h.cmd.Process != nil {
		_ = h.cmd.Process.Kill()
		_, _ = h.cmd.Process.Wait()
	}
	if h.mysqld > 0 {
		_ = syscall.Kill(h.mysqld, syscall.SIGKILL)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.supervisor <= 0 || !processAlive(h.supervisor) {
			if h.mysqld <= 0 || !processAlive(h.mysqld) {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func startHold(t *testing.T, root string) *holdProc {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = roleEnv(root, "hold", "")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	stderr, err := cmd.StderrPipe()
	require.NoError(t, err)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	errBuf := &bytes.Buffer{}
	go func() { io.Copy(errBuf, stderr) }()
	h := &holdProc{cmd: cmd, stdin: stdin}
	sc := bufio.NewScanner(stdout)
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		lineCh := make(chan string, 1)
		go func() {
			if sc.Scan() {
				lineCh <- sc.Text()
				return
			}
			lineCh <- ""
		}()
		select {
		case line := <-lineCh:
			if line == "" {
				cmd.Wait()
				t.Fatalf("hold exited: %s", errBuf.String())
			}
			if line == "ready" {
				t.Cleanup(h.stop)
				return h
			}
			key, val, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			switch key {
			case "socket":
				h.socket = val
			case "mysqld_pid":
				h.mysqld, _ = strconv.Atoi(val)
			case "supervisor_pid":
				h.supervisor, _ = strconv.Atoi(val)
			}
		case <-time.After(time.Until(deadline)):
			cmd.Process.Kill()
			t.Fatalf("hold did not become ready: %s", errBuf.String())
		}
	}
	cmd.Process.Kill()
	t.Fatalf("hold did not become ready: %s", errBuf.String())
	return nil
}

func runTryStopOnce(t *testing.T, root, id string, timeout time.Duration) (string, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = roleEnv(root, "trystop", id)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	lineCh := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		if sc.Scan() {
			lineCh <- sc.Text()
			return
		}
		lineCh <- ""
	}()
	select {
	case line := <-lineCh:
		stdin.Close()
		errBytes, _ := io.ReadAll(stderr)
		waitErr := cmd.Wait()
		if line == "" && waitErr != nil {
			return "", errors.New(strings.TrimSpace(string(errBytes)))
		}
		return line, nil
	case <-time.After(timeout):
		cmd.Process.Kill()
		errBytes, _ := io.ReadAll(stderr)
		return "", errors.New("trystop timed out: " + string(errBytes))
	}
}

func waitExit(t *testing.T, cmd *exec.Cmd, timeout time.Duration) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatal("process did not exit")
	}
}

func waitDead(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("pid %d is still alive", pid)
}

func roleEnv(root, role, runID string) []string {
	env := putEnv(os.Environ(), "MYSQL_ROLE", role)
	env = putEnv(env, "MYSQL_SHARE", "1")
	env = putEnv(env, "MYSQL_SHARE_ROOT", root)
	env = putEnv(env, "MYSQL_SHARE_IDLE", "400ms")
	if runID != "" {
		env = putEnv(env, "MYSQL_SHARE_RUN", runID)
	}
	return env
}

func putEnv(env []string, key, val string) []string {
	prefix := key + "="
	next := make([]string, 0, len(env)+1)
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			continue
		}
		next = append(next, e)
	}
	return append(next, prefix+val)
}
