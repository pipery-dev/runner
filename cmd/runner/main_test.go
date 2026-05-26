package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRegistrationRequiresURL(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := run([]string{"--token", "secret-token-value"}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("exit code = %d", code)
	}
	combined := stdout.String() + stderr.String()
	if strings.Contains(combined, "secret-token-value") {
		t.Fatalf("token leaked in output: %s", combined)
	}
	if !strings.Contains(combined, "--url is required") {
		t.Fatalf("missing validation message: %s", combined)
	}
}

func TestRegistrationURLValidation(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := run([]string{
		"--url", "://bad",
		"--token", "secret-token-value",
	}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("exit code = %d", code)
	}
	if !strings.Contains(stderr.String(), "missing protocol scheme") && !strings.Contains(stderr.String(), "host is required") {
		t.Fatalf("stderr = %s", stderr.String())
	}
}
