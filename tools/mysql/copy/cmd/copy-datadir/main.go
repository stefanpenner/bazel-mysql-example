package main

import (
	"fmt"
	"os"

	"github.com/stefanpenner/bazel-mysql/tools/mysql/copy"
)

func main() {
	if err := copy.Run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
