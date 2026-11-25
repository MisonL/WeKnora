package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestHandleVersionArg(t *testing.T) {
	for _, args := range [][]string{{"--version"}, {"-v"}, {"version"}, {" --version "}} {
		var out bytes.Buffer
		if !handleVersionArg(args, &out) {
			t.Fatalf("args %v must be recognised as a version request", args)
		}
		printed := out.String()
		if !strings.Contains(printed, "WeKnora") || !strings.Contains(printed, "commit:") {
			t.Fatalf("args %v printed %q, want version and commit lines", args, printed)
		}
	}
	for _, args := range [][]string{nil, {}, {"--config", "version"}, {"serve"}, {"-vv"}} {
		var out bytes.Buffer
		if handleVersionArg(args, &out) {
			t.Fatalf("args %v must not be treated as a version request", args)
		}
		if out.Len() != 0 {
			t.Fatalf("args %v printed %q", args, out.String())
		}
	}
}
