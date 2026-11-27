// Command ops-copilot is the single Ops Copilot binary. For now it only
// prints its version; configuration, logging and HTTP come later.
package main

import "fmt"

// version can be set at build time with -ldflags "-X main.version=<v>".
// The build does not set it yet (SU-09 will), so it reports "dev".
var version = "dev"

// versionString returns the program name followed by its version.
func versionString() string {
	return "ops-copilot " + version
}

func main() {
	fmt.Println(versionString())
}
