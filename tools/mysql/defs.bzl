load("@rules_go//go:def.bzl", "go_test")

def _missing(tags, prefixes):
    for tag in tags:
        for prefix in prefixes:
            if tag.startswith(prefix):
                return False
    return True

def mysql_go_test(name, **kwargs):
    """A go_test that execs the MySQL server for the test execution platform."""
    data = list(kwargs.pop("data", []))
    data.append("//tools/mysql:current")
    data.append("//tools/mysql:datadir")
    env = dict(kwargs.pop("env", {}))
    env["MYSQLD"] = "$(rlocationpath //tools/mysql:current)"
    env["MYSQL_DATADIR"] = "$(rlocationpath //tools/mysql:datadir)"
    gotags = list(kwargs.pop("gotags", []))
    if "bazel" not in gotags:
        gotags.append("bazel")
    # Idle RSS in the flag notes is above the 100 MB size default.
    # Reserve 512 MB and 1 CPU. Bazel starts the test only when both fit.
    tags = list(kwargs.pop("tags", []))
    if _missing(tags, ["cpu:", "resources:cpu:"]):
        tags.append("cpu:1")
    if _missing(tags, ["resources:memory:"]):
        tags.append("resources:memory:512")
    kwargs.setdefault("timeout", "short")
    go_test(
        name = name,
        data = data,
        env = env,
        gotags = gotags,
        tags = tags,
        **kwargs
    )
