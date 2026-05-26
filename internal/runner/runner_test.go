package runner

import (
	"bytes"
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/pipery-dev/runner/internal/job"
	"github.com/pipery-dev/runner/internal/result"
)

func TestRunnerRunsScriptAndMasksSecrets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test script uses sh")
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	r := Runner{
		WorkDir: t.TempDir(),
		TempDir: t.TempDir(),
		Stdout:  &stdout,
		Stderr:  &stderr,
	}

	res, err := r.Run(context.Background(), job.Message{
		Variables: map[string]job.VariableValue{
			"SECRET": {Value: "s3cr3t", IsSecret: true},
		},
		MaskHints: []job.MaskHint{{Type: "MaskType.Regex", Value: "token-[0-9]+"}},
		Steps: []job.Step{{
			DisplayName: "test",
			Inputs: map[string]string{
				"shell":  "sh",
				"script": "echo s3cr3t; echo token-123; echo ::notice::ok",
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res != result.Succeeded {
		t.Fatalf("result = %s", res)
	}
	out := stdout.String()
	if strings.Contains(out, "s3cr3t") || strings.Contains(out, "token-123") {
		t.Fatalf("unmasked output: %s", out)
	}
	if !strings.Contains(out, "NOTICE: ok") {
		t.Fatalf("missing notice: %s", out)
	}
}

func TestRunnerFailsOnNonZeroExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test script uses sh")
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	r := Runner{
		WorkDir: t.TempDir(),
		TempDir: t.TempDir(),
		Stdout:  &stdout,
		Stderr:  &stderr,
	}

	res, err := r.Run(context.Background(), job.Message{
		Steps: []job.Step{{
			DisplayName: "fail",
			Inputs: map[string]string{
				"shell":  "sh",
				"script": "exit 4",
			},
		}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if res != result.Failed {
		t.Fatalf("result = %s", res)
	}
}
