package main

import (
	"fmt"
	"os"

	"github.com/stefanpenner/bazel-mysql/test/mysql"
)

func main() {
	if err := mysql.Run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
