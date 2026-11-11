load(":pins.bzl", "ARCHIVES")

TOOLCHAIN_TYPE = "//tools/mysql:toolchain_type"

def _mysql_toolchain_impl(ctx):
    return [platform_common.ToolchainInfo(
        mysqld = ctx.file.mysqld,
        runtime = ctx.files.runtime,
    )]

mysql_toolchain = rule(
    implementation = _mysql_toolchain_impl,
    attrs = {
        "mysqld": attr.label(
            allow_single_file = True,
            mandatory = True,
        ),
        "runtime": attr.label(
            allow_files = True,
            mandatory = True,
        ),
    },
)

def _current_mysql_impl(ctx):
    tool = ctx.toolchains[TOOLCHAIN_TYPE]
    rlocation = tool.mysqld.short_path
    if rlocation.startswith("../"):
        rlocation = rlocation[3:]
    marker = ctx.actions.declare_file(ctx.label.name)
    ctx.actions.write(output = marker, content = rlocation + "\n")
    files = [tool.mysqld] + tool.runtime
    return [DefaultInfo(
        files = depset([marker]),
        runfiles = ctx.runfiles(files = [marker] + files),
    )]

current_mysql = rule(
    implementation = _current_mysql_impl,
    toolchains = [TOOLCHAIN_TYPE],
)

def _shq(value):
    return "'" + value.replace("'", "'\\''") + "'"

def _mysql_datadir_impl(ctx):
    # --no-defaults keeps the action off the host my.cnf.
    # mysqld --initialize-insecure is not bit-stable. The output is a cache of that tool.
    tool = ctx.toolchains[TOOLCHAIN_TYPE]
    out = ctx.actions.declare_directory(ctx.label.name)
    command = "\n".join([
        "set -eu",
        "root=$(pwd)",
        "mysqld=" + _shq(tool.mysqld.path),
        "out=" + _shq(out.path),
        'case "$mysqld" in /*) ;; *) mysqld="$root/$mysqld" ;; esac',
        'case "$out" in /*) ;; *) out="$root/$out" ;; esac',
        'base=$(dirname "$(dirname "$mysqld")")',
        'user_arg=""',
        'if [ "$(id -u)" -eq 0 ]; then',
        '  user_arg="--user=root"',
        "fi",
        'plugin_arg=""',
        'if [ -d "$base/lib/plugin" ]; then',
        '  plugin_arg="--plugin-dir=$base/lib/plugin"',
        'elif [ -d "$base/lib/mysql/plugin" ]; then',
        '  plugin_arg="--plugin-dir=$base/lib/mysql/plugin"',
        "fi",
        'export DYLD_LIBRARY_PATH="$base/lib:$base/bin"',
        'export LD_LIBRARY_PATH="$base/lib:$base/bin"',
        'mkdir -p "$out"',
        # The redo log default is 100 MiB. An 8 MiB log makes the template 97.6 MiB versus 189.6 MiB.
        # Initialize warns that this log is full, and the action still finishes.
        # Doublewrite files add 16 MiB. The test server skips doublewrite, so this action skips it too.
        # The pool, the log buffer, the commit flush, the flush method, and the event scheduler match the test server.
        '"$mysqld" --no-defaults --basedir="$base" --datadir="$out" $plugin_arg $user_arg --innodb-buffer-pool-size=33554432 --innodb-log-buffer-size=8388608 --innodb-redo-log-capacity=8388608 --innodb-flush-log-at-trx-commit=0 --innodb-flush-method=nosync --skip-innodb-doublewrite --event-scheduler=OFF --initialize-insecure',
        'chmod 750 "$out"',
    ])
    ctx.actions.run_shell(
        outputs = [out],
        inputs = tool.runtime,
        tools = [tool.mysqld],
        command = command,
        mnemonic = "MysqlDatadir",
        progress_message = "Initialize MySQL data directory",
    )
    return [DefaultInfo(
        files = depset([out]),
        runfiles = ctx.runfiles(files = [out]),
    )]

mysql_datadir = rule(
    implementation = _mysql_datadir_impl,
    toolchains = [TOOLCHAIN_TYPE],
)

def declare_mysql_toolchains():
    # The MySQL server runs in the test process.
    # The execution platform and the target platform must both match the archive.
    # That match is the CPU check. The test does not read the binary header.
    # The manual tag excludes these targets from `bazel test //...`.
    # A wildcard build would fetch every archive.
    for archive in ARCHIVES:
        mysql_toolchain(
            name = archive["repo"] + "_impl",
            mysqld = "@%s//:bin/mysqld" % archive["repo"],
            runtime = "@%s//:runtime" % archive["repo"],
            tags = ["manual"],
        )
        native.toolchain(
            name = archive["repo"] + "_toolchain",
            exec_compatible_with = archive["exec_compatible_with"],
            target_compatible_with = archive["target_compatible_with"],
            tags = ["manual"],
            toolchain = ":%s_impl" % archive["repo"],
            toolchain_type = TOOLCHAIN_TYPE,
        )
