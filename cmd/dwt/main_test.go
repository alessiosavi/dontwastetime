package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun_Version(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"version"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if !strings.HasPrefix(out.String(), "dwt ") {
		t.Errorf("version output %q", out.String())
	}
}

func TestCLI_DispatchUnknownCommand(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"frobnicate"}, &out, &errb); code != 2 {
		t.Fatalf("unknown command exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "unknown command") || !strings.Contains(errb.String(), "usage") {
		t.Errorf("stderr %q", errb.String())
	}
}

func TestCLI_NoArgsPrintsUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(nil, &out, &errb); code != 2 || !strings.Contains(errb.String(), "usage") {
		t.Fatalf("exit %d stderr %q", code, errb.String())
	}
}
