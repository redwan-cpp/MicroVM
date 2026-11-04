// Command sandboxctl is the request form: the tool you type commands into.
// It never touches a VM itself. It only sends requests to sandboxd.
//
// Week 0: only `sandboxctl version` works. `run`, `ps` and `logs` arrive in
// week 4 (Demo 1).
package main

import (
	"fmt"
	"os"

	"github.com/redwan-cpp/microvm-sandbox/internal/version"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "version" {
		fmt.Println(version.String("sandboxctl"))
		return
	}

	fmt.Fprintln(os.Stderr, "usage: sandboxctl version")
	fmt.Fprintln(os.Stderr, "(run, ps and logs arrive in week 4)")
	os.Exit(2) // exit code 2 is the usual "you used the command wrong"
}
