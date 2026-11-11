load("@rules_go//go:def.bzl", "go_test")

def mysql_go_test(name, **kwargs):
    """A go_test that execs the MySQL server for the test execution platform."""
    data = list(kwargs.pop("data", []))
    data.append("//tools/mysql:current")
    data.append("//tools/mysql:datadir")
    data.append("//tools/mysql:mysqld.cnf")
    env = dict(kwargs.pop("env", {}))
    env["MYSQLD"] = "$(rlocationpath //tools/mysql:current)"
    env["MYSQL_DATADIR"] = "$(rlocationpath //tools/mysql:datadir)"
    env["MYSQL_DEFAULTS_FILE"] = "$(rlocationpath //tools/mysql:mysqld.cnf)"
    gotags = list(kwargs.pop("gotags", []))
    if "bazel" not in gotags:
        gotags.append("bazel")
    kwargs.setdefault("timeout", "short")
    go_test(
        name = name,
        data = data,
        env = env,
        gotags = gotags,
        **kwargs
    )
