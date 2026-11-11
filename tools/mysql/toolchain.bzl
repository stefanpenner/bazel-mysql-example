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
    # mysqld --initialize-insecure is not bit-stable. The output is a cache of that tool.
    # The flag file is an action input, so an edit rebuilds this directory.
    tool = ctx.toolchains[TOOLCHAIN_TYPE]
    out = ctx.actions.declare_directory(ctx.label.name)
    flags = ctx.file.flags
    command = "\n".join([
        "set -eu",
        "root=$(pwd)",
        "mysqld=" + _shq(tool.mysqld.path),
        "out=" + _shq(out.path),
        "flags=" + _shq(flags.path),
        'case "$mysqld" in /*) ;; *) mysqld="$root/$mysqld" ;; esac',
        'case "$out" in /*) ;; *) out="$root/$out" ;; esac',
        'case "$flags" in /*) ;; *) flags="$root/$flags" ;; esac',
        'base=$(dirname "$(dirname "$mysqld")")',
        'user_arg=""',
        'if [ "$(id -u)" -eq 0 ]; then',
        # MySQL refuses to run as root without --user.
        # Some builds run this action as uid 0.
        # Impact: initialize finishes when the uid is 0.
        '  user_arg="--user=root"',
        "fi",
        # plugin-dir is the plugin directory inside basedir.
        # MySQL loads server plugins only from that directory.
        # Impact: initialize does not load a host plugin.
        'plugin_arg=""',
        'if [ -d "$base/lib/plugin" ]; then',
        '  plugin_arg="--plugin-dir=$base/lib/plugin"',
        'elif [ -d "$base/lib/mysql/plugin" ]; then',
        '  plugin_arg="--plugin-dir=$base/lib/mysql/plugin"',
        "fi",
        'export DYLD_LIBRARY_PATH="$base/lib:$base/bin"',
        'export LD_LIBRARY_PATH="$base/lib:$base/bin"',
        'mkdir -p "$out"',
        # --defaults-file is the first argument. MySQL reads that file and no host option file.
        # --basedir is the MySQL install. Initialize reads share files from it.
        # --datadir is this action output. The directory becomes the test template.
        # --initialize-insecure creates the directory and exits. The root account has no password.
        # Impact: each test copies this directory and starts mysqld.
        '"$mysqld" --defaults-file="$flags" --basedir="$base" --datadir="$out" $plugin_arg $user_arg --initialize-insecure',
        'chmod 750 "$out"',
    ])
    ctx.actions.run_shell(
        outputs = [out],
        inputs = tool.runtime + [flags],
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
    attrs = {
        "flags": attr.label(
            allow_single_file = [".cnf"],
            default = "//tools/mysql:mysqld.cnf",
            doc = "The only copy of the mysqld tuning flags.",
        ),
    },
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
