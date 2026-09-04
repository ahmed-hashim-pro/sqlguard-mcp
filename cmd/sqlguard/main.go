// Command sqlguard is an MCP server that gives an LLM agent read access to a
// SQL database while refusing anything it cannot prove is a read.
package main

import (
	"fmt"
	"os"
)

// version is overridden at build time with -ldflags "-X main.version=..."
var version = "dev"

const usage = `sqlguard - a governed SQL gateway for LLM agents

usage:
  sqlguard serve --db <path>    run the MCP server over stdio
  sqlguard approve <token>      approve a pending write, out of band
  sqlguard version              print the version

`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	switch os.Args[1] {
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}
