package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/pipery-dev/runner/internal/job"
	"github.com/pipery-dev/runner/internal/listener"
	"github.com/pipery-dev/runner/internal/register"
	"github.com/pipery-dev/runner/internal/runner"
)

const version = "0.1.0"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("runner", flag.ContinueOnError)
	flags.SetOutput(stderr)

	var jobPath string
	var runnerURL string
	var token string
	var tokenEnv string
	var name string
	var labels string
	var replace bool
	var ephemeral bool
	var workDir string
	var tempDir string
	var dryRun bool
	var runListenerMode bool
	var debug bool
	var showVersion bool

	flags.StringVar(&jobPath, "job", "", "path to an AgentJobRequestMessage-compatible JSON file")
	flags.StringVar(&runnerURL, "url", "", "GitHub repository or organization URL for runner registration")
	flags.StringVar(&token, "token", "", "runner registration token")
	flags.StringVar(&tokenEnv, "token-env", "", "environment variable containing the runner registration token")
	flags.StringVar(&name, "name", "", "runner name")
	flags.StringVar(&labels, "labels", "", "comma-separated user labels")
	flags.BoolVar(&replace, "replace", false, "replace an existing runner with the same name")
	flags.BoolVar(&ephemeral, "ephemeral", false, "register as an ephemeral runner")
	flags.StringVar(&workDir, "work", "", "workspace directory")
	flags.StringVar(&tempDir, "temp", "", "temporary directory for generated scripts")
	flags.BoolVar(&dryRun, "dry-run", false, "print steps without executing them")
	flags.BoolVar(&runListenerMode, "run", false, "start the listener loop after registration or from an existing config")
	flags.BoolVar(&debug, "debug", false, "print debug logs")
	flags.BoolVar(&showVersion, "version", false, "print version")

	if err := flags.Parse(args); err != nil {
		return 2
	}

	if showVersion {
		fmt.Fprintln(stdout, version)
		return 0
	}

	if jobPath == "" {
		if runnerURL != "" || token != "" || tokenEnv != "" {
			if token == "" && tokenEnv != "" {
				token = os.Getenv(tokenEnv)
			}
			code := runRegistration(runnerURL, token, name, labels, workDir, replace, ephemeral, debug, stdout, stderr)
			if code != 0 || !runListenerMode {
				return code
			}
			ctx, stop := signalContext()
			return runConfiguredListener(".", debug, stdout, stderr, ctx, stop)
		}
		if runListenerMode {
			ctx, stop := signalContext()
			return runConfiguredListener(".", debug, stdout, stderr, ctx, stop)
		}
		fmt.Fprintln(stderr, "--job is required")
		return 2
	}

	if workDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(stderr, "get cwd: %v\n", err)
			return 1
		}
		workDir = cwd
	}

	workDir, err := filepath.Abs(workDir)
	if err != nil {
		fmt.Fprintf(stderr, "resolve work dir: %v\n", err)
		return 1
	}

	if tempDir == "" {
		tempDir = filepath.Join(workDir, "_temp")
	}

	message, err := loadJob(jobPath)
	if err != nil {
		fmt.Fprintf(stderr, "load job: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	r := runner.Runner{
		WorkDir: workDir,
		TempDir: tempDir,
		Stdout:  stdout,
		Stderr:  stderr,
		DryRun:  dryRun,
	}

	result, err := r.Run(ctx, message)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(stderr, "runner canceled")
			return 130
		}
		fmt.Fprintf(stderr, "runner failed: %v\n", err)
		return 1
	}

	return result.ExitCode()
}

func runConfiguredListener(root string, debug bool, stdout, stderr io.Writer, ctx context.Context, stop context.CancelFunc) int {
	defer stop()
	l := listener.Listener{
		Root:        root,
		Stdout:      stdout,
		Stderr:      stderr,
		Debug:       debug,
		DebugWriter: stderr,
	}
	if err := l.Run(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(stderr, "listener canceled")
			return 130
		}
		fmt.Fprintf(stderr, "listener failed: %v\n", err)
		return 1
	}
	return 0
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func runRegistration(runnerURL, token, name, rawLabels, workDir string, replace, ephemeral, debug bool, stdout, stderr io.Writer) int {
	if runnerURL == "" {
		fmt.Fprintln(stderr, "--url is required when --token is provided")
		return 2
	}
	if token == "" {
		fmt.Fprintln(stderr, "--token is required when --url is provided")
		return 2
	}
	if err := register.ValidateURL(runnerURL); err != nil {
		fmt.Fprintf(stderr, "invalid --url: %v\n", err)
		return 2
	}
	if workDir == "" {
		workDir = "_work"
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	reg, err := register.NewClient(nil).Register(ctx, register.Options{
		URL:         runnerURL,
		Token:       token,
		Name:        name,
		WorkFolder:  workDir,
		Replace:     replace,
		Ephemeral:   ephemeral,
		Labels:      splitCSV(rawLabels),
		Debug:       debug,
		DebugWriter: stderr,
	})
	if err != nil {
		fmt.Fprintf(stderr, "registration failed: %v\n", err)
		return 1
	}
	if err := register.Save(".", reg); err != nil {
		fmt.Fprintf(stderr, "save registration: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Runner registered: %s (%d)\n", reg.Settings.AgentName, reg.Settings.AgentID)
	fmt.Fprintln(stdout, "Settings saved to .runner and .credentials")
	return 0
}

func splitCSV(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func loadJob(path string) (job.Message, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return job.Message{}, err
	}

	var message job.Message
	if err := json.Unmarshal(data, &message); err != nil {
		return job.Message{}, err
	}
	return message, nil
}
