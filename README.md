# bazel & mysql

## What & Why

An integration test reasonably requires MySQL, but using MySQL
and bazel isn't entirely straightforward, this repo describes a
reasonable approach that combines both bazel integration & tuning for mysql as
one example.

**Impact**

Considerable reduced memory and cpu overhead, and improved wall clock time.
Plus it works natively with bazel.

On my 48 GiB M4 MacBook Pro, the table compares MySQL defaults with these flags.

The disk cache was warm.
Startup time is the median of 5 runs.
Insert time is the median of 7 runs.
A wait, a CPU time, and a memory block are the median of 5 runs.

| | MySQL default | test flags |
| --- | --- | --- |
| data directory | 190 MiB | 82 MiB |
| idle memory | 499.5 MiB | 138.3 MiB |
| memory after 2000 inserts | 520.5 MiB | 149.5 MiB |
| buffer pool | 130.9 MiB | 32.7 MiB |
| log buffer | 64.0 MiB | 8.0 MiB |
| redo link buffer | 24.0 MiB | 24.0 MiB |
| Performance Schema | 234.4 MiB | off |
| server accepts a connection | 368 ms | 320 ms |
| 2000 autocommit inserts | 184 ms | 47 ms |
| redo log wait | 122.8 ms | 2.8 ms |
| binary log wait | 75.2 ms | 0.0 ms |
| connection CPU | 121.2 ms | 35.6 ms |
| log thread CPU | 100.0 ms | 1.9 ms |


The test server uses 361 MiB less memory while it is idle.
2000 inserts take 47 ms instead of 184 ms.
The server accepts a connection in 320 ms instead of 368 ms.
A test of 10 inserts and one read takes about 1 ms in both columns.


## What this repo does

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
[Resolve](tools/mysql/toolchain.bzl#L127-L134)
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
[Files for the test](tools/mysql/defs.bzl#L6-L12)
[Find mysqld](tools/mysql/server/server.go#L281-L299)
[Copy into TEST_TMPDIR](tools/mysql/server/server.go#L39-L52)
[Clone or full copy](tools/mysql/copy/copy.go#L35-L51)
[macOS clone](tools/mysql/copy/clone_darwin.go#L21-L29)
[Linux clone](tools/mysql/copy/clone_linux.go#L14-L52)
[copy-datadir](tools/mysql/copy/cmd/copy-datadir/main.go#L10-L14)

3. Put the tuning flags in one option file.
   The file is `tools/mysql/mysqld.cnf`.
   The data-directory action and the test server both pass that file.
   `--defaults-file` is the first argument.
   MySQL then reads that file and no host option file.
   A change to the file rebuilds the data directory.

   The test process adds four paths.
   Those paths exist only while the test runs.
   The paths are the socket, the pid file, the error log, and `secure_file_priv`.
   The action adds `--initialize-insecure`.
   Both commands add `--basedir`, `--datadir`, and the plugin directory.
   Both commands add `--user=root` when the uid is 0.

   Each option in the file has a comment.
   The comment states the change, the reason, and the impact.
   The redo capacity and `skip_innodb_doublewrite` change files in the template.
   The template size is in the copy section above.

   The tests read the server variables on this private server.
   The tests do not query Performance Schema tables.
   The tests do not open the data directory after a crash.

[Option file](tools/mysql/mysqld.cnf#L1-L79)
[Pass the file to the test server](tools/mysql/server/server.go#L72-L116)
[Pass the file to initialize](tools/mysql/toolchain.bzl#L81-L86)
[Read the variables](tools/mysql/server/server_test.go#L60-L83)
