package main

import (
	"fmt"
	"io"
	"runtime"
	"strings"

	"github.com/Tencent/WeKnora/internal/handler"
)

// versionArgs are the spellings that ask the binary for its build identity.
// They are handled before any configuration, logging or dependency is set up:
// a bare `--version` on a server binary used to fall through to normal startup
// and launch a second instance.
var versionArgs = map[string]bool{
	"--version": true,
	"-v":        true,
	"version":   true,
}

// handleVersionArg prints the build identity when the first argument asks for
// it and reports whether it did. Only the first argument is considered, so a
// later value that happens to be called "version" is not swallowed.
func handleVersionArg(args []string, out io.Writer) bool {
	if len(args) == 0 || !versionArgs[strings.TrimSpace(args[0])] {
		return false
	}
	goVersion := handler.GoVersion
	if goVersion == "" || goVersion == "unknown" {
		goVersion = runtime.Version()
	}
	fmt.Fprintf(out, "WeKnora %s\ncommit: %s\ngo: %s\n", handler.Version, handler.CommitID, goVersion)
	return true
}
