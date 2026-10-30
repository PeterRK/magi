package main

import (
	"os"

	"magi/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
