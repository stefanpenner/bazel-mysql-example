# MySQL Community Server 8.4.13 LTS.
# One archive per execution platform. The toolchain selects one.
# Bazel fetches the selected archive only.
# Oracle publishes no Intel macOS archive for 8.4.13.

VERSION = "8.4.13"

ARCHIVES = [
    {
        "repo": "mysql_darwin_arm64",
        "exec_compatible_with": [
            "@platforms//os:osx",
            "@platforms//cpu:aarch64",
        ],
        "target_compatible_with": [
            "@platforms//os:osx",
            "@platforms//cpu:aarch64",
        ],
        "urls": ["https://cdn.mysql.com/Downloads/MySQL-8.4/mysql-8.4.13-macos15-arm64.tar.gz"],
        "sha256": "f324f8950e24d4147470bbc6a6ef0b90ef93b91d3fc74154b3d2ba085b44912a",
        "strip_prefix": "mysql-8.4.13-macos15-arm64",
    },
    {
        "repo": "mysql_linux_arm64",
        "exec_compatible_with": [
            "@platforms//os:linux",
            "@platforms//cpu:aarch64",
        ],
        "target_compatible_with": [
            "@platforms//os:linux",
            "@platforms//cpu:aarch64",
        ],
        "urls": ["https://cdn.mysql.com/Downloads/MySQL-8.4/mysql-8.4.13-linux-glibc2.28-aarch64.tar.xz"],
        "sha256": "6531bb50f64c986a2abdab6b647cbd553e8740b3e5d3867f5415e33568879fc9",
        "strip_prefix": "mysql-8.4.13-linux-glibc2.28-aarch64",
    },
    {
        "repo": "mysql_linux_amd64",
        "exec_compatible_with": [
            "@platforms//os:linux",
            "@platforms//cpu:x86_64",
        ],
        "target_compatible_with": [
            "@platforms//os:linux",
            "@platforms//cpu:x86_64",
        ],
        "urls": ["https://cdn.mysql.com/Downloads/MySQL-8.4/mysql-8.4.13-linux-glibc2.28-x86_64.tar.xz"],
        "sha256": "e32f82598aef1384d2bbb8e57a1c684f9b7734409ae8028b363fb913e56b8e92",
        "strip_prefix": "mysql-8.4.13-linux-glibc2.28-x86_64",
    },
]
