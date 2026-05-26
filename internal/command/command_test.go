package command

import "testing"

func TestParseV2(t *testing.T) {
	cmd, ok := Parse("::error file=main.go,line=7::bad%0Athing")
	if !ok {
		t.Fatal("expected command")
	}
	if cmd.Name != "error" {
		t.Fatalf("name = %q", cmd.Name)
	}
	if cmd.Properties["file"] != "main.go" || cmd.Properties["line"] != "7" {
		t.Fatalf("properties = %#v", cmd.Properties)
	}
	if cmd.Data != "bad\nthing" {
		t.Fatalf("data = %q", cmd.Data)
	}
}

func TestParseLegacy(t *testing.T) {
	cmd, ok := Parse("##[warning]careful")
	if !ok {
		t.Fatal("expected command")
	}
	if cmd.Name != "warning" || cmd.Data != "careful" {
		t.Fatalf("cmd = %#v", cmd)
	}
}
