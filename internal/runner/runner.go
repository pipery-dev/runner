package runner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/pipery-dev/runner/internal/command"
	"github.com/pipery-dev/runner/internal/job"
	"github.com/pipery-dev/runner/internal/masker"
	"github.com/pipery-dev/runner/internal/result"
	"github.com/pipery-dev/runner/internal/shell"
)

type Runner struct {
	WorkDir string
	TempDir string
	Stdout  io.Writer
	Stderr  io.Writer
	DryRun  bool
}

func (r Runner) Run(ctx context.Context, message job.Message) (result.Result, error) {
	if r.Stdout == nil {
		r.Stdout = io.Discard
	}
	if r.Stderr == nil {
		r.Stderr = io.Discard
	}
	if r.WorkDir == "" {
		r.WorkDir = "."
	}
	if r.TempDir == "" {
		r.TempDir = filepath.Join(r.WorkDir, "_temp")
	}
	if len(message.Steps) == 0 {
		return result.Succeeded, nil
	}

	m, err := buildMasker(message)
	if err != nil {
		return result.Failed, err
	}

	if !r.DryRun {
		if err := os.MkdirAll(r.TempDir, 0o755); err != nil {
			return result.Failed, err
		}
	}

	for i, step := range message.Steps {
		if ctx.Err() != nil {
			return result.Canceled, ctx.Err()
		}
		if err := r.runStep(ctx, m, message, i, step); err != nil {
			if errors.Is(err, context.Canceled) {
				return result.Canceled, err
			}
			return result.Failed, err
		}
	}
	return result.Succeeded, nil
}

func (r Runner) runStep(ctx context.Context, m *masker.Masker, message job.Message, index int, step job.Step) error {
	script := step.Script()
	if script == "" {
		fmt.Fprintf(r.Stdout, "Skipping step %d (%s): no script input\n", index+1, step.Name())
		return nil
	}

	spec, err := shell.Resolve(step.Shell())
	if err != nil {
		return err
	}

	workingDir := r.resolveWorkingDirectory(step.WorkingDirectory())
	if r.DryRun {
		fmt.Fprintf(r.Stdout, "Would run step %d (%s): %s in %s\n", index+1, step.Name(), spec.Command, workingDir)
		return nil
	}

	if err := os.MkdirAll(workingDir, 0o755); err != nil {
		return err
	}

	scriptFile, err := os.CreateTemp(r.TempDir, "step-*"+spec.Extension)
	if err != nil {
		return err
	}
	scriptPath := scriptFile.Name()
	defer os.Remove(scriptPath)

	if _, err := scriptFile.WriteString(shell.FixUp(spec.Command, script)); err != nil {
		scriptFile.Close()
		return err
	}
	if err := scriptFile.Close(); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(scriptPath, 0o700); err != nil {
			return err
		}
	}

	args := strings.ReplaceAll(spec.ArgFormat, "{0}", scriptPath)
	fmt.Fprintf(r.Stdout, "##[group]Run %s\n", m.Mask(firstLine(script)))
	fmt.Fprintf(r.Stdout, "shell: %s %s\n", spec.Path, spec.ArgFormat)
	if env := step.MergedEnv(); len(env) > 0 {
		fmt.Fprintln(r.Stdout, "env:")
		for k, v := range env {
			fmt.Fprintf(r.Stdout, "  %s: %s\n", k, m.Mask(v))
		}
	}
	fmt.Fprintln(r.Stdout, "##[endgroup]")

	cmd := exec.CommandContext(ctx, spec.Path, splitArgs(args)...)
	cmd.Dir = workingDir
	cmd.Env = r.environment(message, step)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	manager := newOutputManager(m, r.Stdout, r.Stderr)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		manager.Scan(stdout, false)
	}()
	go func() {
		defer wg.Done()
		manager.Scan(stderr, true)
	}()

	waitErr := cmd.Wait()
	wg.Wait()
	if waitErr == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return fmt.Errorf("step %q failed with exit code %d", step.Name(), exitErr.ExitCode())
	}
	return waitErr
}

func (r Runner) resolveWorkingDirectory(value string) string {
	if value == "" {
		return r.WorkDir
	}
	if filepath.IsAbs(value) {
		return value
	}
	return filepath.Join(r.WorkDir, value)
}

func (r Runner) environment(message job.Message, step job.Step) []string {
	env := os.Environ()
	values := map[string]string{
		"GITHUB_WORKSPACE": r.WorkDir,
		"RUNNER_TEMP":      r.TempDir,
		"RUNNER_OS":        strings.ToUpper(runtime.GOOS[:1]) + runtime.GOOS[1:],
	}
	for name, variable := range message.Variables {
		values[name] = variable.Value
	}
	for name, value := range step.MergedEnv() {
		values[name] = value
	}
	for name, value := range values {
		env = append(env, name+"="+value)
	}
	return env
}

func buildMasker(message job.Message) (*masker.Masker, error) {
	m := masker.New()
	for name, variable := range message.Variables {
		if !variable.IsSecret {
			continue
		}
		switch strings.ToUpper(name) {
		case "ACTIONS_STEP_DEBUG", "ACTIONS_RUNNER_DEBUG":
			continue
		}
		m.AddValue(variable.Value)
		for _, line := range strings.FieldsFunc(variable.Value, func(r rune) bool { return r == '\r' || r == '\n' }) {
			m.AddValue(line)
		}
	}
	for _, hint := range message.MaskHints {
		if strings.EqualFold(hint.Type, "regex") || strings.EqualFold(hint.Type, "MaskType.Regex") {
			if err := m.AddRegex(hint.Value); err != nil {
				return nil, err
			}
			m.AddValue(hint.Value)
		}
	}
	return m, nil
}

type outputManager struct {
	masker       *masker.Masker
	stdout       io.Writer
	stderr       io.Writer
	stopped      bool
	resumeToken  string
	knownCommand map[string]bool
	mu           sync.Mutex
}

func newOutputManager(m *masker.Masker, stdout, stderr io.Writer) *outputManager {
	return &outputManager{
		masker: m,
		stdout: stdout,
		stderr: stderr,
		knownCommand: map[string]bool{
			"add-mask":      true,
			"debug":         true,
			"error":         true,
			"group":         true,
			"notice":        true,
			"stop-commands": true,
			"warning":       true,
		},
	}
}

func (m *outputManager) Scan(r io.Reader, isErr bool) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		m.HandleLine(scanner.Text(), isErr)
	}
}

func (m *outputManager) HandleLine(line string, isErr bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cmd, ok := command.Parse(line)
	if ok {
		if m.stopped {
			if cmd.Name == strings.ToLower(m.resumeToken) {
				m.stopped = false
				m.resumeToken = ""
				fmt.Fprintln(m.stdout, m.masker.Mask(line))
				return
			}
			m.write(line, isErr)
			return
		}

		switch cmd.Name {
		case "stop-commands":
			m.stopped = true
			m.resumeToken = cmd.Data
			fmt.Fprintln(m.stdout, m.masker.Mask(line))
			return
		case "add-mask":
			m.masker.AddValue(cmd.Data)
			return
		case "error":
			fmt.Fprintf(m.stderr, "ERROR: %s\n", m.masker.Mask(cmd.Data))
			return
		case "warning":
			fmt.Fprintf(m.stdout, "WARNING: %s\n", m.masker.Mask(cmd.Data))
			return
		case "notice":
			fmt.Fprintf(m.stdout, "NOTICE: %s\n", m.masker.Mask(cmd.Data))
			return
		case "debug":
			fmt.Fprintf(m.stdout, "DEBUG: %s\n", m.masker.Mask(cmd.Data))
			return
		}
	}

	m.write(line, isErr)
}

func (m *outputManager) write(line string, isErr bool) {
	line = m.masker.Mask(line)
	if isErr {
		fmt.Fprintln(m.stderr, line)
		return
	}
	fmt.Fprintln(m.stdout, line)
}

func firstLine(script string) string {
	for _, line := range strings.Split(strings.TrimSpace(script), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

func splitArgs(args string) []string {
	var out []string
	var b strings.Builder
	var quote rune
	escaped := false
	for _, r := range args {
		switch {
		case escaped:
			b.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				b.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
		case r == ' ' || r == '\t':
			if b.Len() > 0 {
				out = append(out, b.String())
				b.Reset()
			}
		default:
			b.WriteRune(r)
		}
	}
	if escaped {
		b.WriteRune('\\')
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}
