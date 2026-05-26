package shell

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

type Spec struct {
	Command   string
	Path      string
	ArgFormat string
	Extension string
}

var defaultArgs = map[string]string{
	"cmd":        `/D /E:ON /V:OFF /S /C "CALL "{0}""`,
	"pwsh":       `-command ". '{0}'"`,
	"powershell": `-command ". '{0}'"`,
	"bash":       "--noprofile --norc -e -o pipefail {0}",
	"sh":         "-e {0}",
	"python":     "{0}",
}

var extensions = map[string]string{
	"cmd":        ".cmd",
	"pwsh":       ".ps1",
	"powershell": ".ps1",
	"bash":       ".sh",
	"sh":         ".sh",
	"python":     ".py",
}

func Resolve(option string) (Spec, error) {
	command, argFormat := parseOption(option)
	if command == "" {
		if runtime.GOOS == "windows" {
			command = "pwsh"
			if _, err := exec.LookPath(command); err != nil {
				command = "powershell"
			}
		} else {
			command = "bash"
			if _, err := exec.LookPath(command); err != nil {
				command = "sh"
			}
		}
	}

	if argFormat == "" {
		argFormat = defaultArgs[strings.ToLower(command)]
	}
	if !strings.Contains(argFormat, "{0}") {
		return Spec{}, errors.New("shell arguments must contain {0}")
	}

	path, err := exec.LookPath(command)
	if err != nil {
		return Spec{}, fmt.Errorf("resolve shell %q: %w", command, err)
	}

	ext := extensions[strings.ToLower(command)]
	if ext == "" {
		ext = ".sh"
	}

	return Spec{
		Command:   command,
		Path:      path,
		ArgFormat: argFormat,
		Extension: ext,
	}, nil
}

func FixUp(command, contents string) string {
	switch strings.ToLower(command) {
	case "cmd":
		return "@echo off\n" + contents
	case "powershell", "pwsh":
		return "$ErrorActionPreference = 'stop'\n" + contents + "\nif ((Test-Path -LiteralPath variable:\\LASTEXITCODE)) { exit $LASTEXITCODE }"
	default:
		return contents
	}
}

func parseOption(option string) (string, string) {
	option = strings.TrimSpace(option)
	if option == "" {
		return "", ""
	}
	parts := strings.SplitN(option, " ", 2)
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], strings.TrimSpace(parts[1])
}
