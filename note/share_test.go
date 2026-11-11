//go:build bazel

package note_test

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/stefanpenner/bazel-mysql/test/mysql"
)

const (
	stopNow     = "stop"
	stopHolders = "holders"
	stopStart   = "start-held"
)

type shareChoice int

const (
	sharePrivate shareChoice = iota
	shareAttach
	shareSupervise
)

type serverInfo struct {
	sock          string
	dataDir       string
	mysqldPath    string
	mysqldPid     int
	supervisorPid int
	mysqlHash     string
}

func attachShared() (*server, error) {
	root, err := shareRoot()
	if err != nil {
		return nil, err
	}
	start, err := lockFile(filepath.Join(root, "start.lock"), syscall.LOCK_EX)
	if err != nil {
		return nil, err
	}
	s, choice, err := attachLocked(root)
	if uerr := unlockClose(start); uerr != nil && err == nil {
		err = uerr
	}
	if err != nil {
		return nil, err
	}
	if choice == sharePrivate {
		return startServer()
	}
	return s, nil
}

func attachLocked(root string) (*server, shareChoice, error) {
	id, err := readCurrent(root)
	if err != nil {
		return nil, 0, err
	}
	live := false
	if id != "" {
		live, err = runLive(root, id)
		if err != nil {
			return nil, 0, err
		}
	}
	choice := chooseShare(runtime.GOOS, live)
	switch choice {
	case shareAttach:
		s, err := joinRun(root, id)
		return s, choice, err
	case shareSupervise:
		s, err := createRun(root)
		return s, choice, err
	default:
		return nil, sharePrivate, nil
	}
}

// chooseShare is the Linux rule and the Mac rule.
// A live server is joined. A Mac with no live server starts the supervisor.
// Any other OS with no live server keeps the private server.
func chooseShare(goos string, live bool) shareChoice {
	if live {
		return shareAttach
	}
	if goos == "darwin" {
		return shareSupervise
	}
	return sharePrivate
}

func joinRun(root, id string) (*server, error) {
	runDir := filepath.Join(root, id)
	clients, err := lockFile(filepath.Join(runDir, "clients.lock"), syscall.LOCK_SH)
	if err != nil {
		return nil, err
	}
	info, err := readServerInfo(filepath.Join(runDir, "server.txt"))
	if err != nil {
		unlockClose(clients)
		return nil, err
	}
	if !mysqldHealthy(info) {
		unlockClose(clients)
		return nil, fmt.Errorf("shared mysqld in %s is not healthy", runDir)
	}
	return serverFromInfo(info, runDir, clients), nil
}

func createRun(root string) (*server, error) {
	id, err := newRunID()
	if err != nil {
		return nil, err
	}
	runDir := filepath.Join(root, id)
	if err := mkdir0700(runDir); err != nil {
		return nil, err
	}
	if _, err := checkSocketPath(filepath.Join(runDir, "s.sock")); err != nil {
		os.RemoveAll(runDir)
		return nil, err
	}
	if err := writeCurrent(root, id); err != nil {
		os.RemoveAll(runDir)
		return nil, err
	}
	// Hold the shared lock before the supervisor starts its idle loop.
	// Otherwise the supervisor can stop mysqld before this test connects.
	clients, err := lockFile(filepath.Join(runDir, "clients.lock"), syscall.LOCK_SH)
	if err != nil {
		clearCurrent(root, id)
		os.RemoveAll(runDir)
		return nil, err
	}
	supPid, err := startSupervisorProcess(root, id)
	if err != nil {
		unlockClose(clients)
		clearCurrent(root, id)
		os.RemoveAll(runDir)
		return nil, err
	}
	info, err := waitReadyFile(root, id, supPid)
	if err != nil {
		unlockClose(clients)
		stopProcessGroup(supPid)
		clearCurrent(root, id)
		os.RemoveAll(runDir)
		return nil, err
	}
	return serverFromInfo(info, runDir, clients), nil
}

func serverFromInfo(info serverInfo, runDir string, hold *os.File) *server {
	return &server{
		sock:          info.sock,
		dataDir:       info.dataDir,
		owned:         false,
		shareHold:     hold,
		mysqldPid:     info.mysqldPid,
		supervisorPid: info.supervisorPid,
		runDir:        runDir,
	}
}

func runLive(root, id string) (bool, error) {
	runDir := filepath.Join(root, id)
	if !supervisorAlive(runDir) {
		info, err := readServerInfo(filepath.Join(runDir, "server.txt"))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				os.RemoveAll(runDir)
				return false, nil
			}
			return false, err
		}
		if err := stopIfOurs(info); err != nil {
			return false, err
		}
		if !processAlive(info.mysqldPid) {
			os.RemoveAll(runDir)
		}
		return false, nil
	}
	info, err := readServerInfo(filepath.Join(runDir, "server.txt"))
	if err != nil {
		return false, fmt.Errorf("live supervisor in %s has no server.txt: %w", runDir, err)
	}
	if !mysqldHealthy(info) {
		return false, fmt.Errorf("shared mysqld in %s is not healthy", runDir)
	}
	return true, nil
}

func runSupervisor() int {
	signal.Ignore(syscall.SIGHUP)
	if err := supervise(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func supervise() error {
	root := os.Getenv("MYSQL_SHARE_ROOT")
	id := os.Getenv("MYSQL_SHARE_RUN")
	if root == "" || id == "" {
		return errors.New("supervisor needs MYSQL_SHARE_ROOT and MYSQL_SHARE_RUN")
	}
	if err := supervisorAllowed(runtime.GOOS); err != nil {
		return err
	}
	runDir := filepath.Join(root, id)
	supLock, err := lockFile(filepath.Join(runDir, "supervisor.lock"), syscall.LOCK_EX)
	if err != nil {
		return err
	}
	defer unlockClose(supLock)

	mysqldPath, err := realMysqld()
	if err != nil {
		return err
	}
	template, err := locateDatadir()
	if err != nil {
		return err
	}
	dataDir := filepath.Join(runDir, "data")
	if err := mysql.CopyDatadir(template, dataDir); err != nil {
		return fmt.Errorf("copy data directory into %s: %w", dataDir, err)
	}
	sock := filepath.Join(runDir, "s.sock")
	if _, err := checkSocketPath(sock); err != nil {
		return err
	}
	logPath := filepath.Join(runDir, "mysql.err")
	logFile, err := os.Create(logPath)
	if err != nil {
		return fmt.Errorf("create mysql log %s: %w", logPath, err)
	}
	defer logFile.Close()
	base := filepath.Dir(filepath.Dir(mysqldPath))
	cmd := exec.Command(mysqldPath, mysqldArgs(base, dataDir, sock, filepath.Join(runDir, "mysql.pid"), logPath, runDir)...)
	cmd.Env = serverEnv(base)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start mysqld: %w", err)
	}
	s := &server{
		cmd:     cmd,
		log:     logFile,
		sock:    sock,
		dataDir: dataDir,
		exited:  make(chan struct{}),
		owned:   true,
	}
	go func() {
		s.exitErr = cmd.Wait()
		close(s.exited)
	}()
	if err := s.waitReady(); err != nil {
		s.stopProcess()
		return err
	}
	hash, err := mysqlHash()
	if err != nil {
		s.stopProcess()
		return err
	}
	info := serverInfo{
		sock:          sock,
		dataDir:       dataDir,
		mysqldPath:    mysqldPath,
		mysqldPid:     cmd.Process.Pid,
		supervisorPid: os.Getpid(),
		mysqlHash:     hash,
	}
	if err := writeServerInfo(filepath.Join(runDir, "server.txt"), info); err != nil {
		s.stopProcess()
		return err
	}
	peer, err := startWatchProcess(root, id, runDir)
	if err != nil {
		s.stopProcess()
		return err
	}
	defer peer.Close()

	startLock, clients, err := confirmIdle(root, id)
	if err != nil {
		s.stopProcess()
		return err
	}
	if err := clearCurrent(root, id); err != nil {
		unlockClose(startLock)
		unlockClose(clients)
		s.stopProcess()
		return err
	}
	unlockClose(startLock)
	unlockClose(clients)
	s.stopProcess()
	if err := os.RemoveAll(runDir); err != nil {
		return fmt.Errorf("delete run directory %s: %w", runDir, err)
	}
	return nil
}

func startSupervisorProcess(root, id string) (int, error) {
	if err := supervisorAllowed(runtime.GOOS); err != nil {
		return 0, err
	}
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		"MYSQL_ROLE=supervisor",
		"MYSQL_SHARE_ROOT="+root,
		"MYSQL_SHARE_RUN="+id,
	)
	// Setsid leaves the Bazel process group. The test exit does not kill this process.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	logPath := filepath.Join(root, id, "supervisor.log")
	lf, err := os.Create(logPath)
	if err != nil {
		return 0, fmt.Errorf("create supervisor log %s: %w", logPath, err)
	}
	cmd.Stdout = lf
	cmd.Stderr = lf
	if err := cmd.Start(); err != nil {
		lf.Close()
		return 0, fmt.Errorf("start supervisor: %w", err)
	}
	lf.Close()
	return cmd.Process.Pid, nil
}

func supervisorAllowed(goos string) error {
	if goos != "darwin" {
		return fmt.Errorf("supervisor start on %s is not supported", goos)
	}
	return nil
}

func startWatchProcess(root, id, runDir string) (*os.File, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("open watch pipe: %w", err)
	}
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		"MYSQL_ROLE=watch",
		"MYSQL_SHARE_ROOT="+root,
		"MYSQL_SHARE_RUN="+id,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.ExtraFiles = []*os.File{r}
	logPath := filepath.Join(runDir, "watch.log")
	lf, err := os.Create(logPath)
	if err != nil {
		r.Close()
		w.Close()
		return nil, fmt.Errorf("create watch log %s: %w", logPath, err)
	}
	cmd.Stdout = lf
	cmd.Stderr = lf
	if err := cmd.Start(); err != nil {
		r.Close()
		w.Close()
		lf.Close()
		return nil, fmt.Errorf("start watch: %w", err)
	}
	r.Close()
	lf.Close()
	return w, nil
}

func runWatch() int {
	signal.Ignore(syscall.SIGHUP)
	root := os.Getenv("MYSQL_SHARE_ROOT")
	id := os.Getenv("MYSQL_SHARE_RUN")
	if root == "" || id == "" {
		fmt.Fprintln(os.Stderr, "watch needs MYSQL_SHARE_ROOT and MYSQL_SHARE_RUN")
		return 1
	}
	peer := os.NewFile(3, "supervisor-pipe")
	buf := make([]byte, 1)
	for {
		_, err := peer.Read(buf)
		if err != nil {
			break
		}
	}
	peer.Close()
	// A clean stop deletes the run directory before this read.
	info, err := readServerInfo(filepath.Join(root, id, "server.txt"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0
		}
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := releaseAfterStop(root, id, info); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// releaseAfterStop clears current only after the pid is confirmed and stopped.
// A failed datadir query is an error. current stays, and the pid stays.
func releaseAfterStop(root, id string, info serverInfo) error {
	if err := stopIfOurs(info); err != nil {
		return err
	}
	if processAlive(info.mysqldPid) {
		return fmt.Errorf("mysqld %d on %s is still running", info.mysqldPid, info.sock)
	}
	start, err := lockFile(filepath.Join(root, "start.lock"), syscall.LOCK_EX)
	if err != nil {
		return err
	}
	defer unlockClose(start)
	return clearCurrent(root, id)
}

func confirmIdle(root, id string) (*os.File, *os.File, error) {
	idle, err := shareIdle()
	if err != nil {
		return nil, nil, err
	}
	clientsPath := filepath.Join(root, id, "clients.lock")
	for {
		// The exclusive lock is the empty-holder check. Release it before the wait
		// so a new test can take the shared lock during that minute.
		held, err := lockFile(clientsPath, syscall.LOCK_EX)
		if err != nil {
			return nil, nil, err
		}
		if err := unlockClose(held); err != nil {
			return nil, nil, err
		}
		time.Sleep(idle)
		kind, start, clients, err := acquireStopLocks(root, id)
		if err != nil {
			return nil, nil, err
		}
		if kind == stopNow {
			return start, clients, nil
		}
	}
}

// acquireStopLocks tries both locks once.
// It does not wait for start.lock while it holds clients.lock.
// That wait deadlocks a test that holds start.lock and waits for clients.lock.
func acquireStopLocks(root, id string) (string, *os.File, *os.File, error) {
	clients, err := lockFile(filepath.Join(root, id, "clients.lock"), syscall.LOCK_EX|syscall.LOCK_NB)
	if isWouldBlock(err) {
		return stopHolders, nil, nil, nil
	}
	if err != nil {
		return "", nil, nil, err
	}
	start, err := lockFile(filepath.Join(root, "start.lock"), syscall.LOCK_EX|syscall.LOCK_NB)
	if isWouldBlock(err) {
		if uerr := unlockClose(clients); uerr != nil {
			return "", nil, nil, uerr
		}
		return stopStart, nil, nil, nil
	}
	if err != nil {
		unlockClose(clients)
		return "", nil, nil, err
	}
	if !stopAllowed(true, true) {
		unlockClose(start)
		unlockClose(clients)
		return "", nil, nil, errors.New("stop guard refused while both locks were free")
	}
	return stopNow, start, clients, nil
}

func stopAllowed(startFree, holdersEmpty bool) bool {
	return startFree && holdersEmpty
}

func runHold() int {
	s, err := attachShared()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("socket=%s\n", s.sock)
	fmt.Printf("mysqld_pid=%d\n", s.mysqldPid)
	fmt.Printf("supervisor_pid=%d\n", s.supervisorPid)
	fmt.Printf("ready\n")
	os.Stdout.Sync()
	_, _ = io.Copy(io.Discard, os.Stdin)
	return 0
}

func runTryStop() int {
	root := os.Getenv("MYSQL_SHARE_ROOT")
	id := os.Getenv("MYSQL_SHARE_RUN")
	kind, start, clients, err := acquireStopLocks(root, id)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(kind)
	os.Stdout.Sync()
	if kind == stopNow {
		_, _ = io.Copy(io.Discard, os.Stdin)
		unlockClose(start)
		unlockClose(clients)
	}
	return 0
}

func runIdleProbe() int {
	root := os.Getenv("MYSQL_SHARE_ROOT")
	id := os.Getenv("MYSQL_SHARE_RUN")
	start, clients, err := confirmIdle(root, id)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	unlockClose(start)
	unlockClose(clients)
	fmt.Println("stop")
	return 0
}

func runSleep() int {
	_, _ = io.Copy(io.Discard, os.Stdin)
	return 0
}

func shareRoot() (string, error) {
	if v := os.Getenv("MYSQL_SHARE_ROOT"); v != "" {
		if err := mkdir0700(v); err != nil {
			return "", err
		}
		return v, nil
	}
	base, err := findOutputBase()
	if err != nil {
		return "", err
	}
	sum, err := mysqlHash()
	if err != nil {
		return "", err
	}
	root := filepath.Join("/tmp/bzmysql", hash16([]byte(base)), sum)
	if err := mkdirAll0700(root); err != nil {
		return "", err
	}
	return root, nil
}

func findOutputBase() (string, error) {
	tmp := os.Getenv("TEST_TMPDIR")
	if base, err := outputBaseFrom(tmp); err == nil {
		return base, nil
	}
	run := os.Getenv("RUNFILES_DIR")
	if base, err := outputBaseFrom(run); err == nil {
		return base, nil
	}
	return "", fmt.Errorf("find output base from TEST_TMPDIR %q and RUNFILES_DIR %q: no execroot", tmp, run)
}

func outputBaseFrom(start string) (string, error) {
	if start == "" {
		return "", errors.New("find output base: path is empty")
	}
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("find output base from %s: %w", start, err)
	}
	// A sandboxed action stores execroot under sandbox/<os>-sandbox/<n>/.
	// That number changes for every action. The output base is the parent of sandbox.
	sawExecroot := false
	var execrootParent string
	for {
		switch filepath.Base(dir) {
		case "execroot":
			sawExecroot = true
			execrootParent = filepath.Dir(dir)
		case "sandbox":
			if sawExecroot {
				return filepath.Dir(dir), nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if execrootParent == "" {
		return "", fmt.Errorf("find output base from %s: no execroot", start)
	}
	return execrootParent, nil
}

func checkSocketPath(path string) (string, error) {
	if len(path) >= 104 {
		return "", fmt.Errorf("socket path %s is %d bytes", path, len(path))
	}
	return path, nil
}

func readCurrent(root string) (string, error) {
	b, err := os.ReadFile(filepath.Join(root, "current"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read current in %s: %w", root, err)
	}
	id := strings.TrimSpace(string(b))
	if id == "" {
		return "", nil
	}
	if len(id) != 16 || !isHex(id) {
		return "", fmt.Errorf("current in %s names %q", root, id)
	}
	return id, nil
}

func writeCurrent(root, id string) error {
	tmp := filepath.Join(root, "current.tmp")
	if err := os.WriteFile(tmp, []byte(id+"\n"), 0600); err != nil {
		return fmt.Errorf("write current in %s: %w", root, err)
	}
	if err := os.Rename(tmp, filepath.Join(root, "current")); err != nil {
		return fmt.Errorf("rename current in %s: %w", root, err)
	}
	return nil
}

func clearCurrent(root, id string) error {
	path := filepath.Join(root, "current")
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read current in %s: %w", root, err)
	}
	if strings.TrimSpace(string(b)) != id {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete current in %s: %w", root, err)
	}
	return nil
}

func newRunID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("read run id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func waitReadyFile(root, id string, supPid int) (serverInfo, error) {
	runDir := filepath.Join(root, id)
	deadline := time.Now().Add(60 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		if !processAlive(supPid) {
			return serverInfo{}, fmt.Errorf("supervisor %d exited before ready in %s\n%s", supPid, runDir, readTail(filepath.Join(runDir, "supervisor.log")))
		}
		info, err := readServerInfo(filepath.Join(runDir, "server.txt"))
		if err == nil && mysqldHealthy(info) {
			return info, nil
		}
		last = err
		time.Sleep(100 * time.Millisecond)
	}
	return serverInfo{}, fmt.Errorf("shared mysqld in %s did not become ready: %v\n%s", runDir, last, readTail(filepath.Join(runDir, "supervisor.log")))
}

func readServerInfo(path string) (serverInfo, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return serverInfo{}, fmt.Errorf("read %s: %w", path, err)
	}
	return parseServerInfo(string(b))
}

func parseServerInfo(text string) (serverInfo, error) {
	info := serverInfo{}
	seen := map[string]bool{}
	body := strings.TrimSpace(text)
	if body == "" {
		return serverInfo{}, errors.New("parse server.txt: file is empty")
	}
	for line := range strings.SplitSeq(body, "\n") {
		key, val, ok := strings.Cut(line, "=")
		if !ok || key == "" || val == "" {
			return serverInfo{}, fmt.Errorf("parse server.txt line %q", line)
		}
		if seen[key] {
			return serverInfo{}, fmt.Errorf("parse server.txt duplicate %s", key)
		}
		seen[key] = true
		switch key {
		case "socket":
			info.sock = val
		case "datadir":
			info.dataDir = val
		case "mysqld":
			info.mysqldPath = val
		case "mysqld_pid":
			n, err := strconv.Atoi(val)
			if err != nil {
				return serverInfo{}, fmt.Errorf("parse server.txt mysqld_pid %q: %w", val, err)
			}
			info.mysqldPid = n
		case "supervisor_pid":
			n, err := strconv.Atoi(val)
			if err != nil {
				return serverInfo{}, fmt.Errorf("parse server.txt supervisor_pid %q: %w", val, err)
			}
			info.supervisorPid = n
		case "mysql_hash":
			info.mysqlHash = val
		default:
			return serverInfo{}, fmt.Errorf("parse server.txt unknown key %s", key)
		}
	}
	for _, key := range []string{"socket", "datadir", "mysqld", "mysqld_pid", "supervisor_pid", "mysql_hash"} {
		if !seen[key] {
			return serverInfo{}, fmt.Errorf("parse server.txt missing %s", key)
		}
	}
	return info, nil
}

func writeServerInfo(path string, info serverInfo) error {
	var b strings.Builder
	fmt.Fprintf(&b, "socket=%s\n", info.sock)
	fmt.Fprintf(&b, "datadir=%s\n", info.dataDir)
	fmt.Fprintf(&b, "mysqld=%s\n", info.mysqldPath)
	fmt.Fprintf(&b, "mysqld_pid=%d\n", info.mysqldPid)
	fmt.Fprintf(&b, "supervisor_pid=%d\n", info.supervisorPid)
	fmt.Fprintf(&b, "mysql_hash=%s\n", info.mysqlHash)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s: %w", path, err)
	}
	return nil
}

func supervisorAlive(runDir string) bool {
	f, err := lockFile(filepath.Join(runDir, "supervisor.lock"), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		return isWouldBlock(err)
	}
	unlockClose(f)
	return false
}

func mysqldHealthy(info serverInfo) bool {
	return mysqldIsOurs(info) && datadirMatches(info.sock, info.dataDir)
}

func mysqldIsOurs(info serverInfo) bool {
	cmd, err := processCommand(info.mysqldPid)
	if err != nil {
		return false
	}
	if !strings.Contains(cmd, "--datadir="+info.dataDir) {
		return false
	}
	base := filepath.Base(info.mysqldPath)
	return base != "" && strings.Contains(cmd, base)
}

func datadirMatches(sock, want string) bool {
	got, ok := queryDatadir(sock)
	return ok && samePath(got, want)
}

func queryDatadir(sock string) (string, bool) {
	db, err := sql.Open("mysql", rootSocketDSN(sock, ""))
	if err != nil {
		return "", false
	}
	defer db.Close()
	var got string
	if err := db.QueryRow("SELECT @@datadir").Scan(&got); err != nil {
		return "", false
	}
	return got, true
}

func stopIfOurs(info serverInfo) error {
	if !mysqldIsOurs(info) {
		return nil
	}
	got, ok := queryDatadir(info.sock)
	if !ok {
		return fmt.Errorf("read datadir for pid %d on socket %s", info.mysqldPid, info.sock)
	}
	if !samePath(got, info.dataDir) {
		return nil
	}
	return stopPid(info.mysqldPid)
}

func stopPid(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("stop pid %d", pid)
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("sigterm %d: %w", pid, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("sigkill %d: %w", pid, err)
	}
	return nil
}

func stopProcessGroup(pid int) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil
}

func processCommand(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("ps pid %d", pid)
	}
	out, err := exec.Command("/bin/ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return "", fmt.Errorf("ps %d: %w", pid, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func samePath(a, b string) bool {
	a = strings.TrimRight(a, `/\`)
	b = strings.TrimRight(b, `/\`)
	ar, err1 := filepath.EvalSymlinks(a)
	br, err2 := filepath.EvalSymlinks(b)
	if err1 == nil && err2 == nil {
		return ar == br
	}
	return a == b
}

func lockFile(path string, how int) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		return nil, fmt.Errorf("flock %s: %w", path, err)
	}
	return f, nil
}

func unlockClose(f *os.File) error {
	if f == nil {
		return nil
	}
	name := f.Name()
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	cerr := f.Close()
	if err != nil {
		return fmt.Errorf("unlock %s: %w", name, err)
	}
	if cerr != nil {
		return fmt.Errorf("close %s: %w", name, cerr)
	}
	return nil
}

func isWouldBlock(err error) bool {
	return errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK)
}

func shareIdle() (time.Duration, error) {
	v := os.Getenv("MYSQL_SHARE_IDLE")
	if v == "" {
		return time.Minute, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("parse MYSQL_SHARE_IDLE %q: %w", v, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("MYSQL_SHARE_IDLE %s is not positive", v)
	}
	return d, nil
}

func realMysqld() (string, error) {
	p, err := locateMysqld()
	if err != nil {
		return "", err
	}
	rp, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("realpath mysqld %s: %w", p, err)
	}
	return rp, nil
}

var (
	mysqlHashOnce sync.Once
	mysqlHashVal  string
	mysqlHashErr  error
)

func mysqlHash() (string, error) {
	mysqlHashOnce.Do(func() {
		mysqlHashVal, mysqlHashErr = computeMysqlHash()
	})
	return mysqlHashVal, mysqlHashErr
}

func computeMysqlHash() (string, error) {
	h := sha256.New()
	mysqld, err := realMysqld()
	if err != nil {
		return "", err
	}
	if err := hashFile(h, mysqld); err != nil {
		return "", fmt.Errorf("hash mysqld %s: %w", mysqld, err)
	}
	template, err := locateDatadir()
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(template)
	if err != nil {
		return "", fmt.Errorf("realpath data directory %s: %w", template, err)
	}
	if err := hashTree(h, resolved); err != nil {
		return "", fmt.Errorf("hash data directory %s: %w", resolved, err)
	}
	base := filepath.Dir(filepath.Dir(mysqld))
	if _, err := io.WriteString(h, flagMaterial(base)); err != nil {
		return "", fmt.Errorf("hash server flags: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)[:8]), nil
}

func flagMaterial(base string) string {
	var b strings.Builder
	for _, flag := range stableFlags() {
		b.WriteString(flag)
		b.WriteByte('\n')
	}
	if plugin := pluginDir(base); plugin != "" {
		b.WriteString(plugin)
		b.WriteByte('\n')
	}
	if os.Geteuid() == 0 {
		b.WriteString("user=root\n")
	}
	return b.String()
}

func hashFile(h io.Writer, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	return nil
}

func hashTree(h io.Writer, root string) error {
	var paths []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(paths)
	for _, path := range paths {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(h, rel); err != nil {
			return err
		}
		if _, err := io.WriteString(h, "\n"); err != nil {
			return err
		}
		if err := hashFile(h, path); err != nil {
			return err
		}
	}
	return nil
}

func hash16(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func isHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return s != ""
}

func mkdir0700(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return fmt.Errorf("mkdir %s: %w", path, err)
	}
	if err := os.Chmod(path, 0700); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}

func mkdirAll0700(path string) error {
	var parts []string
	cur := path
	for {
		parts = append(parts, cur)
		parent := filepath.Dir(cur)
		if parent == cur || parent == "/tmp" || parent == "/" {
			break
		}
		cur = parent
	}
	for i := len(parts) - 1; i >= 0; i-- {
		if err := os.Mkdir(parts[i], 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("mkdir %s: %w", parts[i], err)
		}
		if err := os.Chmod(parts[i], 0700); err != nil {
			return fmt.Errorf("chmod %s: %w", parts[i], err)
		}
	}
	return nil
}

func rootSocketDSN(sock, db string) string {
	return fmt.Sprintf("root@unix(%s)/%s?parseTime=true&charset=utf8mb4&timeout=5s", sock, db)
}

func userDSN(sock, user, pass, db string) string {
	return fmt.Sprintf("%s:%s@unix(%s)/%s?parseTime=true&charset=utf8mb4&timeout=5s", user, pass, sock, db)
}

func sharedNames(testName string) (string, string, string, error) {
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", "", "", fmt.Errorf("read tenant id: %w", err)
	}
	suffix := hex.EncodeToString(rnd[:])
	sum := sha256.Sum256([]byte(os.Args[0] + "\n" + testName + "\n" + suffix))
	db := "t_" + hex.EncodeToString(sum[:8]) + "_" + suffix
	user := "u_" + hex.EncodeToString(sum[:8])
	return db, user, suffix, nil
}

func grantTenant(admin *sql.DB, testName string) (string, string, string, error) {
	db, user, pass, err := sharedNames(testName)
	if err != nil {
		return "", "", "", err
	}
	if _, err := admin.Exec("CREATE DATABASE `" + db + "`"); err != nil {
		return "", "", "", fmt.Errorf("create database %s: %w", db, err)
	}
	create := fmt.Sprintf("CREATE USER '%s'@'localhost' IDENTIFIED BY '%s'", user, pass)
	if _, err := admin.Exec(create); err != nil {
		return "", "", "", fmt.Errorf("create user %s: %w", user, err)
	}
	grant := fmt.Sprintf("GRANT ALL ON `%s`.* TO '%s'@'localhost'", db, user)
	if _, err := admin.Exec(grant); err != nil {
		return "", "", "", fmt.Errorf("grant %s on %s: %w", user, db, err)
	}
	return db, user, pass, nil
}

func dropTenant(t interface{ Errorf(string, ...any) }, admin *sql.DB, db, user string) {
	if _, err := admin.Exec("DROP DATABASE `" + db + "`"); err != nil {
		t.Errorf("drop database %s: %v", db, err)
	}
	if _, err := admin.Exec("DROP USER '" + user + "'@'localhost'"); err != nil {
		t.Errorf("drop user %s: %v", user, err)
	}
}

func readTail(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(b) > 4000 {
		b = b[len(b)-4000:]
	}
	return string(b)
}
