#!/bin/sh
# install.sh — install the `horde` CLI from GitHub Releases.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/jorge-barreto/horde/main/scripts/install.sh | sh
#
# Environment overrides:
#   HORDE_VERSION   Tag to install (e.g. v0.1.0). Defaults to the latest release.
#   HORDE_BINDIR    Directory to install into. Defaults to /usr/local/bin if
#                   writable, otherwise $HOME/.local/bin.
#
# This script is intentionally POSIX sh (no bashisms) so it runs under the
# minimal /bin/sh that `curl ... | sh` typically invokes.

set -eu

# ----------------------------------------------------------------------------
# Configuration
# ----------------------------------------------------------------------------
REPO="jorge-barreto/horde"
BINARY="horde"
API_URL="https://api.github.com/repos/${REPO}/releases/latest"

# ----------------------------------------------------------------------------
# Helpers
# ----------------------------------------------------------------------------

# err: print a message to stderr and exit non-zero.
err() {
	echo "error: $*" >&2
	exit 1
}

# info: print a status message to stderr (stdout is kept clean-ish, though we
# don't pipe stdout anywhere — using stderr keeps messages visible even if a
# caller captures stdout).
info() {
	echo "$*" >&2
}

# have: true if a command exists on PATH.
have() {
	command -v "$1" >/dev/null 2>&1
}

# fetch: download a URL to a destination file using curl or wget.
# Usage: fetch <url> <dest>
fetch() {
	_url="$1"
	_dest="$2"
	if have curl; then
		# -f: fail on HTTP errors, -sS: quiet but show errors, -L: follow redirects.
		curl -fsSL "$_url" -o "$_dest"
	elif have wget; then
		wget -q "$_url" -O "$_dest"
	else
		err "neither curl nor wget is available; cannot download $_url"
	fi
}

# fetch_stdout: download a URL and emit its contents to stdout.
fetch_stdout() {
	_url="$1"
	if have curl; then
		curl -fsSL "$_url"
	elif have wget; then
		wget -q "$_url" -O -
	else
		err "neither curl nor wget is available; cannot download $_url"
	fi
}

# ----------------------------------------------------------------------------
# Detect OS (uname -s -> Go GOOS)
# ----------------------------------------------------------------------------
os_raw="$(uname -s)"
case "$os_raw" in
Linux) OS="linux" ;;
Darwin) OS="darwin" ;;
*)
	err "unsupported operating system: $os_raw
Pre-built binaries are only published for Linux and macOS.
Build from source (Go 1.24+): go install github.com/jorge-barreto/horde/cmd/horde@latest
or download a release manually from https://github.com/${REPO}/releases"
	;;
esac

# ----------------------------------------------------------------------------
# Detect arch (uname -m -> Go GOARCH)
# ----------------------------------------------------------------------------
arch_raw="$(uname -m)"
case "$arch_raw" in
x86_64 | amd64) ARCH="amd64" ;;
arm64 | aarch64) ARCH="arm64" ;;
*)
	err "unsupported architecture: $arch_raw
Pre-built binaries are only published for amd64 (x86_64) and arm64 (aarch64).
Build from source (Go 1.24+): go install github.com/jorge-barreto/horde/cmd/horde@latest
or download a release manually from https://github.com/${REPO}/releases"
	;;
esac

# ----------------------------------------------------------------------------
# Resolve the release tag.
#
# Honor HORDE_VERSION if set; otherwise ask the GitHub API for the latest
# release and parse its tag_name. We parse with grep/sed (no jq dependency):
# the API returns a line like   "tag_name": "v0.1.0",
# ----------------------------------------------------------------------------
if [ -n "${HORDE_VERSION:-}" ]; then
	TAG="$HORDE_VERSION"
	info "using HORDE_VERSION override: $TAG"
else
	info "resolving latest release of ${REPO} ..."
	# Extract the first tag_name value. grep -o keeps only the matched JSON
	# fragment; sed then pulls out the quoted value.
	TAG="$(
		fetch_stdout "$API_URL" |
			grep -o '"tag_name"[[:space:]]*:[[:space:]]*"[^"]*"' |
			head -n 1 |
			sed -E 's/.*"tag_name"[[:space:]]*:[[:space:]]*"([^"]*)".*/\1/'
	)" || TAG=""
	[ -n "$TAG" ] || err "could not determine the latest release tag from $API_URL
You can pin a version explicitly, e.g.: HORDE_VERSION=v0.1.0 sh install.sh"
	info "latest release is $TAG"
fi

# ----------------------------------------------------------------------------
# Compute the asset version (tag without the leading v).
#
# GoReleaser strips a leading 'v' from the tag when naming assets, so tag
# v0.1.0 -> asset version 0.1.0. We also defensively strip a 'cli-v' prefix.
# Order: drop 'cli-v' first, then a bare leading 'v'.
# ----------------------------------------------------------------------------
ASSET_VERSION="$(echo "$TAG" | sed -E 's/^cli-v//; s/^v//')"
[ -n "$ASSET_VERSION" ] || err "could not derive asset version from tag '$TAG'"

# ----------------------------------------------------------------------------
# Build URLs.
#
# The download path uses the FULL tag (with the v): .../download/v0.1.0/...
# The asset filename uses the stripped version:      horde_0.1.0_linux_amd64.tar.gz
# ----------------------------------------------------------------------------
ASSET="${BINARY}_${ASSET_VERSION}_${OS}_${ARCH}.tar.gz"
BASE_URL="https://github.com/${REPO}/releases/download/${TAG}"
ASSET_URL="${BASE_URL}/${ASSET}"
CHECKSUMS_URL="${BASE_URL}/checksums.txt"

info "installing ${BINARY} ${TAG} for ${OS}/${ARCH}"

# ----------------------------------------------------------------------------
# Download into a temp dir that we always clean up.
# ----------------------------------------------------------------------------
tmp="$(mktemp -d)"
# shellcheck disable=SC2064
# Expand $tmp now so the trap removes the right directory even if tmp changes.
trap "rm -rf \"$tmp\"" EXIT INT TERM

info "downloading $ASSET ..."
fetch "$ASSET_URL" "$tmp/$ASSET" ||
	err "failed to download $ASSET_URL
Check that release '$TAG' exists and includes an asset for ${OS}/${ARCH}."

info "downloading checksums.txt ..."
fetch "$CHECKSUMS_URL" "$tmp/checksums.txt" ||
	err "failed to download $CHECKSUMS_URL"

# ----------------------------------------------------------------------------
# Verify the checksum.
#
# checksums.txt lines look like:  <sha256>  horde_0.1.0_linux_amd64.tar.gz
# We pull the line for our asset and pipe it to a checker in -c (check) mode.
# Run from inside the temp dir so the relative filename in the line resolves.
#
# Subshell exit semantics: the outer `|| err` checks the subshell's exit
# status, so every branch must encode pass/fail in that status.
#   - sha256sum / shasum: their -c exit status (0 match, non-zero mismatch)
#     propagates as the subshell's status — the outer guard reports a mismatch.
#   - no tool, no opt-out: we FAIL CLOSED. We print the specific guidance to
#     stderr here and `exit 1`, letting the outer `|| err` abort with its
#     generic message. We deliberately do NOT call err() from inside the
#     subshell (err exits the subshell non-zero, which the outer `|| err` would
#     then DOUBLE-report). One specific message + the outer abort = one clear
#     failure.
#   - no tool, opt-out set: warn and `exit 0` so the install proceeds.
# ----------------------------------------------------------------------------
(
	cd "$tmp"
	if have sha256sum; then
		grep " ${ASSET}\$" checksums.txt | sha256sum -c -
	elif have shasum; then
		# macOS / BSD: shasum -a 256 provides the same -c behavior.
		grep " ${ASSET}\$" checksums.txt | shasum -a 256 -c -
	elif [ -n "${HORDE_INSECURE_SKIP_CHECKSUM:-}" ]; then
		echo "warning: skipping checksum verification by request (HORDE_INSECURE_SKIP_CHECKSUM set)" >&2
	else
		echo "error: no sha256sum/shasum found to verify download; install 'coreutils' or re-run with HORDE_INSECURE_SKIP_CHECKSUM=1 to bypass" >&2
		exit 1
	fi
) || err "checksum verification failed for $ASSET"

# ----------------------------------------------------------------------------
# Extract the single `horde` binary from the tarball.
# ----------------------------------------------------------------------------
info "extracting $BINARY ..."
tar -xzf "$tmp/$ASSET" -C "$tmp" "$BINARY" ||
	tar -xzf "$tmp/$ASSET" -C "$tmp" # fall back to extracting everything
[ -f "$tmp/$BINARY" ] || err "archive $ASSET did not contain a '$BINARY' executable"

# ----------------------------------------------------------------------------
# Choose the install directory.
#
# HORDE_BINDIR wins. Otherwise prefer /usr/local/bin if it is writable, else
# fall back to ~/.local/bin (created if missing).
# ----------------------------------------------------------------------------
if [ -n "${HORDE_BINDIR:-}" ]; then
	BINDIR="$HORDE_BINDIR"
	mkdir -p "$BINDIR" || err "could not create install directory: $BINDIR"
elif [ -w /usr/local/bin ]; then
	BINDIR="/usr/local/bin"
else
	BINDIR="$HOME/.local/bin"
	mkdir -p "$BINDIR" || err "could not create install directory: $BINDIR"
fi

# ----------------------------------------------------------------------------
# Install the binary (0755). Prefer `install`; fall back to cp + chmod.
# ----------------------------------------------------------------------------
if have install; then
	install -m 0755 "$tmp/$BINARY" "$BINDIR/$BINARY" ||
		err "failed to install $BINARY into $BINDIR (permission denied?)"
else
	cp "$tmp/$BINARY" "$BINDIR/$BINARY" ||
		err "failed to copy $BINARY into $BINDIR (permission denied?)"
	chmod 0755 "$BINDIR/$BINARY" ||
		err "failed to set permissions on $BINDIR/$BINARY"
fi

info "installed $BINARY to $BINDIR/$BINARY"

# ----------------------------------------------------------------------------
# Warn if the install dir is not on PATH.
#
# We pad PATH with ':' on both ends so a simple substring test cannot match a
# directory by accident (e.g. /bin matching /usr/bin).
# ----------------------------------------------------------------------------
case ":${PATH}:" in
*":${BINDIR}:"*) ;;
*)
	info ""
	info "note: $BINDIR is not on your PATH."
	info "Add it, for example by appending this line to your shell profile:"
	info "  export PATH=\"$BINDIR:\$PATH\""
	;;
esac

# ----------------------------------------------------------------------------
# Show the installed version (best effort).
# ----------------------------------------------------------------------------
"$BINDIR/$BINARY" --version || true
