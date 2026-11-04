// Command sandboxd is the control-room operator: a daemon (a program that runs
// in the background all the time) that creates, runs and destroys microVMs.
// It is the only part of the system that talks to Firecracker.
//
// Week 0: only `sandboxd --version` works. The real daemon arrives in week 4.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/redwan-cpp/MicroVM/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String("sandboxd"))
		return
	}

	fmt.Fprintln(os.Stderr, "sandboxd: not built yet (arrives in week 4); try --version")
	os.Exit(1)
}
