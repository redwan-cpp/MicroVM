// Command sandbox-agent is the assistant that lives inside each microVM.
// It listens on the vsock "intercom", runs the command it's given, and sends
// the output back to sandboxd.
//
// It is built as a static binary (CGO_ENABLED=0 in the Makefile), so it runs
// inside the tiny guest without needing any other files.
//
// Week 0: only `sandbox-agent --version` works. The real agent arrives in
// week 2.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/redwan-cpp/microvm-sandbox/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String("sandbox-agent"))
		return
	}

	fmt.Fprintln(os.Stderr, "sandbox-agent: not built yet (arrives in week 2); try --version")
	os.Exit(1)
}
