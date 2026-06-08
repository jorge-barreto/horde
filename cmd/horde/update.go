package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/urfave/cli/v3"
)

// normalizeVersion strips a cli-v / v prefix and returns a bare x.y.z, or ""
// for anything that isn't a clean three-part numeric version (dev builds,
// git-describe strings, garbage). Callers treat "" as "not comparable".
// Pre-release tags (e.g. v1.2.3-rc1) are deliberately treated as not-a-clean-version
// (return "") because horde only ships clean x.y.z releases via GoReleaser.
func normalizeVersion(v string) string {
	v = strings.TrimPrefix(v, "cli-v")
	v = strings.TrimPrefix(v, "v")
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return ""
	}
	for _, p := range parts {
		if p == "" {
			return ""
		}
		if _, err := strconv.Atoi(p); err != nil {
			return ""
		}
	}
	return v
}

// isNewer reports whether latest > current under x.y.z ordering. Inputs are
// normalized first; if either is not a clean version, returns false (a dev
// build never auto-claims an upgrade is available).
func isNewer(current, latest string) bool {
	c := normalizeVersion(current)
	l := normalizeVersion(latest)
	if c == "" || l == "" {
		return false
	}
	cp := strings.Split(c, ".")
	lp := strings.Split(l, ".")
	for i := 0; i < 3; i++ {
		cn, _ := strconv.Atoi(cp[i])
		ln, _ := strconv.Atoi(lp[i])
		if ln != cn {
			return ln > cn
		}
	}
	return false
}

// parseLatestTag extracts tag_name from a GitHub releases/latest response.
func parseLatestTag(body []byte) (string, error) {
	var r struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return "", fmt.Errorf("parsing release JSON: %w", err)
	}
	if r.TagName == "" {
		return "", fmt.Errorf("no tag_name in release response")
	}
	return r.TagName, nil
}

const (
	latestReleaseURL = "https://api.github.com/repos/jorge-barreto/horde/releases/latest"
	installScriptURL = "https://raw.githubusercontent.com/jorge-barreto/horde/main/scripts/install.sh"
)

func fetchLatestTag(ctx context.Context, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestReleaseURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "horde/"+version)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("querying latest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("latest release: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return parseLatestTag(body)
}

func updateCmd() *cli.Command {
	return &cli.Command{
		Name:  "update",
		Usage: "Update horde to the latest release",
		Description: `Checks GitHub for the latest horde CLI release and, if newer than the
running binary, downloads and installs it via the official install script.
Use --check to only report whether an update is available.

Homebrew users should run 'brew upgrade horde' instead.`,
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "check", Usage: "Only report if a newer version exists; don't install"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			latest, err := fetchLatestTag(ctx, 10*time.Second)
			if err != nil {
				return err
			}
			cur := version
			if !isNewer(cur, latest) {
				fmt.Fprintf(cmd.Writer, "horde is up to date (%s).\n", cur)
				return nil
			}
			fmt.Fprintf(cmd.Writer, "A newer horde is available: %s (current: %s).\n", latest, cur)
			if cmd.Bool("check") {
				fmt.Fprintln(cmd.Writer, "Run 'horde update' to install it.")
				return nil
			}
			fmt.Fprintln(cmd.Writer, "Installing...")
			// Mirror scripts/install.sh: prefer curl, fall back to wget, so `horde update`
			// works on minimal images that ship only one of them.
			const downloader = `if command -v curl >/dev/null 2>&1; then curl -fsSL "$0"; ` +
				`elif command -v wget >/dev/null 2>&1; then wget -qO- "$0"; ` +
				`else echo "horde update: need curl or wget" >&2; exit 1; fi | sh`
			sh := exec.CommandContext(ctx, "sh", "-c", downloader, installScriptURL)
			sh.Stdout = os.Stdout
			sh.Stderr = os.Stderr
			sh.Env = append(os.Environ(), "HORDE_VERSION="+latest)
			if err := sh.Run(); err != nil {
				return fmt.Errorf("running installer: %w", err)
			}
			return nil
		},
	}
}

// newerVersionNote returns a one-line upgrade note if a newer release exists,
// or "" on any error/equal/dev build. Best-effort: short timeout, never errors
// to the caller. Suppressed in CI or when HORDE_NO_UPDATE_CHECK is set.
func newerVersionNote(ctx context.Context) string {
	if os.Getenv("CI") != "" || os.Getenv("HORDE_NO_UPDATE_CHECK") != "" {
		return ""
	}
	// short timeout: a passive nicety must not stall the version command
	latest, err := fetchLatestTag(ctx, 2*time.Second)
	if err != nil || !isNewer(version, latest) {
		return ""
	}
	return fmt.Sprintf("A newer horde is available: %s (run 'horde update').", latest)
}

// versionString is the shared version descriptor: "<version> (<commit>, built <date>)".
// urfave/cli renders the --version flag as "horde version " + versionString(); the
// `version` subcommand prints "horde version " + versionString() itself. Single
// source so the two can never drift.
func versionString() string {
	return fmt.Sprintf("%s (%s, built %s)", version, commit, buildDate)
}

func versionCmd() *cli.Command {
	return &cli.Command{
		Name:  "version",
		Usage: "Print the horde version (and note any available update)",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			fmt.Fprintf(cmd.Writer, "horde version %s\n", versionString())
			if note := newerVersionNote(ctx); note != "" {
				fmt.Fprintln(cmd.Writer, note)
			}
			return nil
		},
	}
}
