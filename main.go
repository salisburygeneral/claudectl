// Command claudectl launches an ephemeral, sandboxed Claude Code session inside
// an Apple `container` (Virtualization.framework) Linux container. It mounts only
// the current project directory and injects the user's Claude subscription
// credentials (read from the macOS Keychain) into the container, so that
// `claude --dangerously-skip-permissions` can be run safely.
//
// The tool is fully non-interactive: everything is driven by flags/env, all
// diagnostics go to stderr, and the container's exit code is propagated.
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
)

// Injected at build time via -ldflags.
var (
	version = "dev"
	commit  = "none"
)

const (
	defaultImage    = "ghcr.io/salisburygeneral/claude:latest"
	keychainService = "Claude Code-credentials"
)

// credentials mirrors the JSON stored in the macOS Keychain item and consumed
// by Linux Claude Code at ~/.claude/.credentials.json.
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
		// flag prints the error and usage already for parse failures.
		return 2
	}

	if *versionFlag {
		fmt.Printf("claudectl %s (commit %s)\n", version, commit)
		return 0
	}

	// Resolve the project directory to an absolute path.
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

	// Determine the image: flag wins over env, env wins over the default.
	image := *imageFlag
	if image == "" {
		image = os.Getenv("CLAUDECTL_IMAGE")
	}
	if image == "" {
		image = defaultImage
	}

	// Read and validate credentials from the Keychain.
	payload, err := readCredentials()
	if err != nil {
		fmt.Fprintf(os.Stderr, "claudectl: %v\n", err)
		fmt.Fprintln(os.Stderr, "claudectl: log in to Claude Code on this host first (run `claude` and complete sign-in), then retry.")
		return 1
	}

	// Stage credentials in a per-session temp dir mounted into the container.
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

	// Build the container invocation.
	args := []string{
		"run", "--rm", "-it",
		"-v", absDir + ":/workspace",
		"-v", tmpDir + ":/home/claude/.claude",
		"-w", "/workspace",
		image,
		"claude", "--dangerously-skip-permissions",
	}
	args = append(args, fs.Args()...)

	cmd := exec.Command("container", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Forward interrupt/terminate signals to the container process rather than
	// letting them kill claudectl outright, so cleanup still runs. Claude Code's
	// TUI needs to receive these to shut down cleanly.
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

// readCredentials reads the Claude Code OAuth payload from the macOS Keychain,
// validates that it contains a non-empty access token, and returns the raw JSON
// bytes verbatim (so the exact payload can be staged for the container).
func readCredentials() ([]byte, error) {
	out, err := exec.Command("security", "find-generic-password", "-s", keychainService, "-w").Output()
	if err != nil {
		return nil, fmt.Errorf("could not read Keychain item %q: %v", keychainService, err)
	}
	// `security -w` appends a trailing newline; trim it for clean JSON.
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

func trimTrailingNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
