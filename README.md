# bazel & mysql

## How to use

Run the tests in this repo.

```
bazel test //...
```

Bazel fetches the MySQL archive for the platform where the test runs.

A Go test that needs MySQL uses `mysql_go_test`.

[Test rule](tools/mysql/defs.bzl#L3-L21)

Gazelle writes that rule for a `go_test`.

[Gazelle map](BUILD.bazel#L4-L4)

The rule sets `MYSQLD` and `MYSQL_DATADIR`.

`MYSQLD` names the `mysqld` binary for this platform.
`MYSQL_DATADIR` names the data directory template.

Start one server for the test binary.

1. Read `MYSQLD` and `MYSQL_DATADIR`.
2. Copy the template into `TEST_TMPDIR`.
3. Start `mysqld`.
4. Create one database for each test.

The `note` package does these steps.

[Start the server](note/server_test.go#L37-L155)
[One server per binary](note/note_test.go#L37-L46)
[One database per test](note/note_test.go#L128-L142)

## What & Why

An integration test reasonable requires MySQL, but using MySQL
and bazel isn't entirely straightforward, this repo describes a
reasonable approach.

What this repo does:

1. Select the right MySQL archive for the execution platform the given test using mysql will run on.
   The host platform does not select the archive.
   Each MySQL variant becomes a toolchain (darwin_aarch64, linux-x86,e tc)

   Each toolchain sets `exec_compatible_with` and
   `target_compatible_with`.

   The two lists name one OS and one CPU.

   Bazel resolves the toolchain when both platforms match
   those lists.

   If the platforms differ, no toolchain matches.

   The module extension fetches only the matching archive.

```
host platform                does not select the archive

target platform              execution platform
the test binary is for this  the test runs here
          \                         /
           both lists must name
           that OS and CPU
                    |
                    v
           module extension fetches
           only that archive
```

Impact. On this Mac, `bazel test //...` fetched only the macOS arm64 archive.
But if the Mac runs it's tests in Linux RBE, no macOS arm64 mysql is needed,
only the mysql_linux_amd64.

[Pins](tools/mysql/pins.bzl#L8-L51)
[Fetch](tools/mysql/extensions.bzl#L28-L35)
[Resolve](tools/mysql/toolchain.bzl#L110-L114)
[Register](MODULE.bazel#L23-L37)

2. Copy a data directory for each test that needs it's own MySQL.
   Runfiles are the read-only files Bazel gives to the test.
   The MySQL server files are runfiles.
   The MySql server requires a writable data directory.
   Bazel sets `TEST_TMPDIR` to a private writable directory.
   The test copies the data directory into `TEST_TMPDIR` for the MySQL process to use.
   The copy is a clone when the disk supports a clone, to reduce waste.
   If the disk does not support a clone, the test copies
   every file.
   `copy-datadir` in `tools/mysql/copy` does this copy.

```
runfiles                          TEST_TMPDIR
-------------------------         ----------------------
archive bin/mysqld                data/
data directory                    clone, or a full copy
read-only                         mysql.err
                                  mysql.pid

socket: /tmp/bzmysql*/s.sock
```

Impact. The data directory is 81.6 MiB.
On this Mac, with a warm disk cache and a 190 MB directory,
a full copy took 82 ms and wrote 190 MB.
An APFS clone took 25 ms and wrote 0.2 MB.

[Marker](tools/mysql/toolchain.bzl#L25-L36)
[Files for the test](tools/mysql/defs.bzl#L6-L10)
[Find mysqld](note/server_test.go#L212-L230)
[Copy into TEST_TMPDIR](note/server_test.go#L43-L56)
[Clone or full copy](tools/mysql/copy/copy.go#L35-L51)
[macOS clone](tools/mysql/copy/clone_darwin.go#L21-L29)
[Linux clone](tools/mysql/copy/clone_linux.go#L14-L52)
[copy-datadir](tools/mysql/copy/cmd/copy-datadir/main.go#L10-L14)

3. Set test flags on the MySQL server.
   The tests do not read Performance Schema.
   The tests do not open the data directory after a crash.

```
MySQL default                   test flag
--------------------------      ---------------------------
Performance Schema on           --skip-performance-schema
binary log on                   --skip-log-bin
doublewrite on                  --skip-innodb-doublewrite
flush each commit               flush_log_at_trx_commit=0
checkpoint fsync                flush_method=nosync
```

Impact. On this 48 GiB Mac, all of these flags together versus MySQL defaults.
The disk cache was warm.
A startup time is the median of 5 runs.
An insert time is the median of 7 runs.

| | MySQL default | test flags |
| --- | --- | --- |
| data directory | 190 MiB | 82 MiB |
| idle memory | 499.5 MiB | 138.3 MiB |
| memory after 2000 inserts | 520.5 MiB | 149.5 MiB |
| server accepts a connection | 368 ms | 320 ms |
| 2000 autocommit inserts | 184 ms | 47 ms |

The test server uses 361 MiB less memory while it is idle.
2000 inserts take 47 ms instead of 184 ms.
The server accepts a connection in 320 ms instead of 368 ms.
A small test, 10 inserts and one read, takes about 1 ms either way.

[Test server flags](note/server_test.go#L83-L128)
[Data directory flags](tools/mysql/toolchain.bzl#L72-L76)
