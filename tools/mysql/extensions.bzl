load("@bazel_tools//tools/build_defs/repo:http.bzl", "http_archive")
load(":pins.bzl", "ARCHIVES", "VERSION")

_BUILD = """
exports_files(
    ["bin/mysqld"],
    visibility = ["//visibility:public"],
)

filegroup(
    name = "runtime",
    srcs = glob([
        "bin/**",
        "lib/**",
        "share/**",
    ]),
    visibility = ["//visibility:public"],
)
"""

def _mysql_impl(ctx):
    versions = []
    for mod in ctx.modules:
        for tag in mod.tags.server:
            versions.append(tag.version)
    if versions != [VERSION]:
        fail("mysql.server version must be %s. Got: %s" % (VERSION, versions))
    for archive in ARCHIVES:
        http_archive(
            name = archive["repo"],
            urls = archive["urls"],
            sha256 = archive["sha256"],
            strip_prefix = archive["strip_prefix"],
            build_file_content = _BUILD,
        )
    return ctx.extension_metadata(
        reproducible = True,
        root_module_direct_deps = [],
        root_module_direct_dev_deps = [archive["repo"] for archive in ARCHIVES],
    )

mysql = module_extension(
    implementation = _mysql_impl,
    tag_classes = {
        "server": tag_class(
            attrs = {
                "version": attr.string(mandatory = True),
            },
        ),
    },
)
