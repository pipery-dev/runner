package shell

import "testing"

func TestFixUpPowerShell(t *testing.T) {
	got := FixUp("pwsh", "Write-Host ok")
	if got == "Write-Host ok" {
		t.Fatal("expected powershell prelude")
	}
}

func TestResolveExplicitFormatRequiresPlaceholder(t *testing.T) {
	_, err := Resolve("sh -c")
	if err == nil {
		t.Fatal("expected missing placeholder error")
	}
}
