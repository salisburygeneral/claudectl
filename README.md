# claudectl

`claudectl` launches an ephemeral, sandboxed [Claude Code](https://claude.com/claude-code) session inside an Apple [`container`](https://github.com/apple/container) (Virtualization.framework) Linux container. It mounts only the current project directory into the container and injects your Claude subscription credentials from the macOS Keychain, so you can run `claude --dangerously-skip-permissions` without exposing the rest of your machine. Each invocation stages the credentials in a per-session temporary directory that is removed when the session ends (including on Ctrl-C), so concurrent sessions in different directories never collide and no long-lived token is written to disk.

## Prerequisites

- macOS with Apple's `container` CLI installed and running:
  ```sh
  brew install container
  container system start
  ```
- You must have signed in to Claude Code on this host at least once (run `claude` and complete sign-in) so that the OAuth token exists in your Keychain.

## Install

Download a prebuilt binary from the [GitHub Releases](https://github.com/salisburygeneral/claudectl/releases) page and place it on your `PATH`, or install from source:

```sh
go install github.com/salisburygeneral/claudectl@latest
```

## Usage

```
claudectl [--dir D] [--image I] [--version] [-- args passed to claude]
```

- `--dir` — project directory to mount at `/workspace` (default: current directory).
- `--image` — container image to run (default: `ghcr.io/salisburygeneral/claude:latest`). Also settable via the `CLAUDECTL_IMAGE` environment variable; the flag wins over the env var.
- `--version` — print version and commit, then exit.
- Anything after `--` is passed through to `claude` inside the container.

### Examples

Run a sandboxed session in the current directory:

```sh
claudectl
```

Run against a specific project directory:

```sh
claudectl --dir ~/code/my-project
```

Pass extra arguments through to `claude`:

```sh
claudectl -- --model claude-opus-4-8
```

Use a custom image:

```sh
claudectl --image ghcr.io/salisburygeneral/claude:latest
# or
CLAUDECTL_IMAGE=ghcr.io/salisburygeneral/claude:latest claudectl
```

## How it works

1. Reads your Claude Code OAuth token from the macOS Keychain item `Claude Code-credentials`.
2. Writes that payload verbatim to `<tmp>/.credentials.json` (mode `0600`) in a fresh temporary directory.
3. Runs:
   ```sh
   container run --rm -it \
     -v <dir>:/workspace \
     -v <tmp>:/home/claude/.claude \
     -w /workspace \
     <image> \
     claude --dangerously-skip-permissions [your args]
   ```
4. Propagates the container's exit code and removes the temporary directory when the session ends. The credentials directory is mounted read-write so in-session token refresh works; the refreshed token is intentionally discarded with the temp directory.
