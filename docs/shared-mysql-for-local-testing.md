# [IDEA] Shared MySQL for local testing

One `bazel test` command on a Mac can share one `mysqld`.
Each test target stays a separate Bazel result.
The code is in `note/share_test.go`.
Today, each test binary starts one private `mysqld`.

The private server stays the default.
A test shares `mysqld` only when `MYSQL_SHARE=1`.

```sh
bazel test --config=mysql_share //...
```

Put this line in `.bazelrc`.

```text
test:mysql_share --test_env=MYSQL_SHARE=1
```

Do not put `MYSQL_SHARE` on the default test line.
The config sets `--test_env=MYSQL_SHARE=1`.
Bazel makes a UUID for each command.
Bazel does not copy that UUID into the test.
`TEST_TMPDIR` is a different directory for each test action.
A test action is one program Bazel runs for one test target.
A test creates the run id only when `current` does not name a live server.
Other tests read that run id from `current`.

## Why this is local

The shared `mysqld` is one process on one machine.
The socket is a file under `/tmp` on that machine.
A test on another machine cannot open that file.

On macOS, the supervisor continues after the test exits.
The Linux test runner kills that supervisor.
A remote worker is a different machine, or it uses the Linux runner.
Use the private server for a remote test and for a Linux test.

## Processes

The supervisor is a small program in this repo.
The supervisor is not part of MySQL.
The supervisor is not part of Bazel.
The watch process is a second program.
The watch process stops `mysqld` if the supervisor exits.

`setsid` puts a process in a new session.
A process group is the set of processes Bazel kills with one test.
The supervisor calls `setsid`, so it is not in that process group.

```text
Bazel owns one process group per test
  process-wrapper
    note_test or store_test

launchd owns the shared session
  supervisor
    mysqld
  watch process
```

The first test starts the supervisor and the watch process.
Both call `setsid`.
`launchd` becomes the parent.
The supervisor starts `mysqld` as its child.
The supervisor starts with the lock files closed.
The watch process and the supervisor share a pipe.

The macOS sandbox can stay at its default.
`sandbox-exec` does not use a PID namespace.
The Seatbelt rules stay on the supervisor.
Those rules permit writes under `/tmp`.

A trial with Bazel 9.3.2 left this child alive.
The trial used `process-wrapper` and `sandbox-exec`.
After the wrapper exited, `launchd` was the parent.
The child could still write a file under `/tmp`.

## Directory

Every test builds one directory from the workspace, the mysql hash, and the run id.

```text
/tmp/bzmysql/<workspace-hash>/<mysql-hash>/
  start.lock
  current
  <run-id>/
    clients.lock
    supervisor.lock
    server.txt
    s.sock
    data/
```

The workspace hash is 16 hex characters from the Bazel output base path.
Bazel names that path from the workspace path.
A sandboxed test skips the per-action sandbox directory.
The mysql hash is 16 hex characters.
The run id is 16 hex characters.
The mysql hash covers three inputs:

- the bytes of the `mysqld` file
- the bytes of the data-directory template
- the server flags

`current` names the live run id.
The first test writes `current` when no server is live.
`server.txt` holds the socket path, the pids, and the mysql hash.
The supervisor holds `supervisor.lock` until it exits.
A test that takes that lock sees a dead supervisor.
The directory mode is `0700`.
Another account on the machine cannot open the socket.

`mysqld` must use the real MySQL install path.
Bazel deletes the test sandbox directory when the test ends.
A path inside that directory disappears with the test.
The socket and the data directory stay under `/tmp`.
The socket path is `/tmp/bzmysql/<16 hex>/<16 hex>/<16 hex>/s.sock`.
That path is 70 bytes.
macOS rejects a Unix socket path of 104 bytes or more.

## How a test connects

The test locks `start.lock`.
The test reads `current`.

If `current` names a live server, the test uses that run id.
If `current` does not name a live server, the test does these steps.

1. The test writes a new run id to `current`.
2. The test starts the supervisor.
3. The supervisor copies the template into `data/` once.
4. The test waits for a socket that accepts a connection.

The test takes a shared lock on `clients.lock`.
The test then unlocks `start.lock`.
The test holds that shared lock until the test exits.
The test connects to the socket in `server.txt`.
As `root`, the test creates one database and one user.
The database name is a hash of the program path, the full test name, and a random suffix.
The user name is a hash of those same three values.
The store connects as that user.
On exit, the test drops that database and that user.
The kernel releases the shared lock when the test process exits.

The user has all rights on that database only.
The user does not get these rights:

- `SYSTEM_VARIABLES_ADMIN`
- `CONNECTION_ADMIN`
- `FILE`
- `SHUTDOWN`
- `PROCESS`
- `RELOAD`
- `SUPER`

## What one server does not separate

A crash of `mysqld` stops every test on that server.
A full disk stops every test on that server.
The buffer pool, the redo log, and the connections are one pool.
`GET_LOCK` names are global on the server.
`SHOW GLOBAL STATUS` is visible to every user.
Startup flags apply to the whole server.

`TestPerformanceSchemaIsOff` reads those flags as `root`.
`TestServerUsesSmallSettings` reads those flags as `root`.
Those two tests pass only when the shared server uses the same flags.

Rows in one database stay out of the other databases.
Temp tables, transactions, and user variables stay on one connection.

## How the server stops

The supervisor takes an exclusive lock on `clients.lock`.
That lock means no test holds the file.
The supervisor releases the lock.
The supervisor waits one minute.
The supervisor tries the exclusive lock again.
If a test holds the lock, the supervisor starts again.
If the lock succeeds, the supervisor tries `start.lock` without waiting.
If a test holds `start.lock`, the supervisor releases `clients.lock` and starts again.
If both locks succeed, the supervisor deletes `current` when it names this run.
The supervisor releases both locks.
The supervisor sends `SIGTERM` to `mysqld`.
If `mysqld` ignores `SIGTERM` for 10 seconds, the supervisor sends `SIGKILL`.
The supervisor deletes the run directory and exits.

One minute must be longer than the pause between tests in one command.
A shorter wait stops `mysqld` while that command still has tests.

If the supervisor exits, the watch process stops `mysqld`.
If the watch process exits, the supervisor stops `mysqld`.
Before that stop, read `SELECT @@datadir`.
Also check that the pid is this `mysqld`.
Do not kill a pid that fails those two checks.

A kill of the supervisor alone still stops `mysqld`.
The process leak needs both programs gone before the stop.
`SIGKILL` of both programs can cause that leak.
A crash of both programs can cause that leak.
The leak also happens if the watch process never starts.
The next test stops a `mysqld` under `/tmp/bzmysql/` whose supervisor is dead.

## When a new server starts

A new `mysqld` file changes the mysql hash.
A new data-directory template changes the mysql hash.
A new server flag changes the mysql hash.
Tests with the new hash start a new `mysqld`.
The old supervisor sees an idle lock and stops the old `mysqld`.
Two `mysqld` processes can run during that minute.
The two processes do not share a data directory.

After the idle minute, `current` no longer names a live server.
The next command creates a new run id and a new `mysqld`.
That command does not read the previous databases.
A command that starts during the idle minute joins the old server.

## Linux

Do not use the Mac procedure on Linux.

`linux-sandbox` puts the test in a PID namespace.
A PID namespace gives the test its own process ids.
When the test exits, the kernel kills every process in that namespace.
After `setsid`, the process stays in that namespace.

The Linux process wrapper also kills a child that called `setsid`.
The wrapper reads `/proc/thread-self/children`.
The wrapper sends `SIGKILL` to each child in that list.
The `local` strategy does not use a PID namespace.
The process wrapper still sends that `SIGKILL`.
No sandbox flag stops that kill.

On Linux, a test does not start the supervisor.
If `current` names a live server, the test connects to that server.
If `current` does not name a live server, the test starts a private `mysqld`.

A shared server on Linux needs an owner that is not a child of the test.
That owner starts `mysqld` and writes `current`.
The owner runs `bazel test --config=mysql_share`.
The owner stops `mysqld` when Bazel exits.
Bazel does not own that `mysqld`, so the Linux kill does not hit it.
The directory, the hash, the user, and the database rules stay the same.
