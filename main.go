package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"unsafe"
)

var (
	version = "dev"
	commit  = "none"
)

const (
	defaultImage    = "ghcr.io/salisburygeneral/claude:latest"
	keychainService = "Claude Code-credentials"
)

type credentials struct {
	ClaudeAiOauth struct {
		AccessToken  string   `json:"accessToken"`
		RefreshToken string   `json:"refreshToken"`
		ExpiresAt    int64    `json:"expiresAt"`
		Scopes       []string `json:"scopes"`
	} `json:"claudeAiOauth"`
}

func main() {
	os.Exit(run())
}

func run() int {
	fs := flag.NewFlagSet("claudectl", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `claudectl - run an ephemeral, sandboxed Claude Code session in an Apple container

Usage:
  claudectl [--dir D] [--image I] [--version] [-- args passed to claude]

Flags:
  --dir string     project directory to mount at /workspace (default: current directory)
  --image string   container image to run (default: %s)
                   overridable via CLAUDECTL_IMAGE; the flag wins over the env var
  --version        print version and commit, then exit

Any arguments after "--" are passed through to `+"`claude`"+` inside the container.
`, defaultImage)
	}

	var (
		dirFlag     = fs.String("dir", "", "project directory to mount at /workspace (default: current directory)")
		imageFlag   = fs.String("image", "", "container image to run")
		versionFlag = fs.Bool("version", false, "print version and commit, then exit")
	)

	if err := fs.Parse(os.Args[1:]); err != nil {
		return 2
	}

	if *versionFlag {
		fmt.Printf("claudectl %s (commit %s)\n", version, commit)
		return 0
	}

	dir := *dirFlag
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "claudectl: cannot determine current directory: %v\n", err)
			return 1
		}
		dir = cwd
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "claudectl: cannot resolve directory %q: %v\n", dir, err)
		return 1
	}
	if info, err := os.Stat(absDir); err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "claudectl: %q is not a directory\n", absDir)
		return 1
	}

	image := *imageFlag
	if image == "" {
		image = os.Getenv("CLAUDECTL_IMAGE")
	}
	if image == "" {
		image = defaultImage
	}

	payload, err := readCredentials()
	if err != nil {
		fmt.Fprintf(os.Stderr, "claudectl: %v\n", err)
		fmt.Fprintln(os.Stderr, "claudectl: log in to Claude Code on this host first (run `claude` and complete sign-in), then retry.")
		return 1
	}

	tmpDir, err := os.MkdirTemp("", "claudectl-creds-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "claudectl: cannot create temp dir: %v\n", err)
		return 1
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }
	defer cleanup()

	credPath := filepath.Join(tmpDir, ".credentials.json")
	if err := os.WriteFile(credPath, payload, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "claudectl: cannot write credentials: %v\n", err)
		return 1
	}

	args := []string{"run", "--rm", "-i"}
	if isTerminal(os.Stdin) {
		args = append(args, "-t")
	}
	args = append(args,
		"-v", absDir+":/workspace",
		"-v", tmpDir+":/home/claude/.claude",
		"-w", "/workspace",
		image,
		"claude", "--dangerously-skip-permissions",
	)
	args = append(args, fs.Args()...)

	cmd := exec.Command("container", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "claudectl: failed to start container: %v\n", err)
		fmt.Fprintln(os.Stderr, "claudectl: is Apple `container` installed and running? Try `brew install container` and `container system start`.")
		return 1
	}

	go func() {
		for sig := range sigCh {
			if cmd.Process != nil {
				_ = cmd.Process.Signal(sig)
			}
		}
	}()

	err = cmd.Wait()
	signal.Stop(sigCh)
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if code := exitErr.ExitCode(); code >= 0 {
				return code
			}
			return 1
		}
		fmt.Fprintf(os.Stderr, "claudectl: container run failed: %v\n", err)
		fmt.Fprintln(os.Stderr, "claudectl: is Apple `container` installed and running? Try `brew install container` and `container system start`.")
		return 1
	}
	return 0
}

func readCredentials() ([]byte, error) {
	out, err := exec.Command("security", "find-generic-password", "-s", keychainService, "-w").Output()
	if err != nil {
		return nil, fmt.Errorf("could not read Keychain item %q: %v", keychainService, err)
	}
	payload := trimTrailingNewline(out)
	if len(payload) == 0 {
		return nil, fmt.Errorf("Keychain item %q is empty", keychainService)
	}

	var creds credentials
	if err := json.Unmarshal(payload, &creds); err != nil {
		return nil, fmt.Errorf("could not parse Keychain credentials as JSON: %v", err)
	}
	if creds.ClaudeAiOauth.AccessToken == "" {
		return nil, fmt.Errorf("Keychain credentials are missing claudeAiOauth.accessToken")
	}
	return payload, nil
}

func isTerminal(f *os.File) bool {
	var termios syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(),
		syscall.TIOCGETA, uintptr(unsafe.Pointer(&termios)))
	return errno == 0
}

func trimTrailingNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
