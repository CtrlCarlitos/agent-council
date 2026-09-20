// Council's bootstrap command reports capabilities truthfully. It does not run agents.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "service" {
		if err := handleServiceCommand(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if len(os.Args) == 2 && (os.Args[1] == "about" || os.Args[1] == "--version") {
		fmt.Println("Agent Council — bootstrap foundation")
		fmt.Println("Implemented: in-memory domain contracts and validators.")
		fmt.Println("Planned: durable service, native sessions, MCP, skill integration, optional UI.")
		return
	}
	fmt.Fprintln(os.Stderr, "usage: council about | --version | service <run|start|status|stop>")
	os.Exit(2)
}
