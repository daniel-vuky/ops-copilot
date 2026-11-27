package main

import "testing"

func TestVersionString(t *testing.T) {
	if got, want := versionString(), "ops-copilot dev"; got != want {
		t.Fatalf("versionString() = %q, want %q", got, want)
	}
}
