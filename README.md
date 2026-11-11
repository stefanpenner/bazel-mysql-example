# bazel & mysql

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
   Bazel sets `TEST_TMPDIR` to a private writable directory.
   The test copies the data directory into `TEST_TMPDIR`.
   The copy is a clone when the disk supports a clone.
   If the disk does not support a clone, the test copies
   every file.
   `copy-datadir` in `test/mysql` does this copy.

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
[Clone or full copy](test/mysql/copy.go#L35-L51)
[macOS clone](test/mysql/clone_darwin.go#L21-L29)
[Linux clone](test/mysql/clone_linux.go#L14-L52)
[copy-datadir](test/mysql/cmd/copy-datadir/main.go#L10-L14)

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

Impact. Each row is a separate measurement on this 48 GiB Mac.
A time is the median of 2000 autocommit inserts.

| Solution | before | after |
| --- | --- | --- |
| Performance Schema off | idle 499.5 MiB | idle 258.4 MiB |
| doublewrite off | idle 254.3 MiB | idle 196.3 MiB |
| doublewrite off | 161.6 ms | 86.6 ms |
| commit flush 0, fsync held | 29.8 ms | 6.7 ms |
| nosync, commit flush 2 held | 29.8 ms | 26.5 ms |
| commit flush 0 and nosync | 6.7 ms | 7.1 ms |
| binary log off | not measured | not measured |

[Test server flags](note/server_test.go#L83-L128)
[Data directory flags](tools/mysql/toolchain.bzl#L72-L76)
