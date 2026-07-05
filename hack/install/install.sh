#!/bin/sh
# Orkano on-box installer.
#
# Downloads the signed orkano CLI, VERIFIES it, and runs `orkano init --local` to
# install a hardened single-node cluster on THIS machine. The heavy, audited work
# lives in the Go binary (ADR-0017); this script only fetches, verifies, and execs
# it — keep it thin.
#
#   curl -fsSL https://get.orkano.io | sh
#
# Prefer to read it before running (recommended for anything piped into a shell):
#
#   curl -fsSLO https://get.orkano.io/install.sh
#   less install.sh
#   sh install.sh
#
# Flags after `--` are passed to `orkano init --local`, e.g.
#   curl -fsSL https://get.orkano.io | sh -s -- --allow-repo you/app
#
# Environment overrides: ORKANO_VERSION (default "latest"), ORKANO_REPO
# (default "orkanoio/orkano").
#
# The artifact names below are pinned by .goreleaser.yaml: the `cli-binary`
# archive publishes bare orkano_<os>_<arch> binaries, `checksum` publishes
# checksums.txt, and `signs` publishes the checksums.txt.sigstore.json cosign
# bundle. Change them together or the installer 404s.
set -eu

ORKANO_VERSION="${ORKANO_VERSION:-latest}"
ORKANO_REPO="${ORKANO_REPO:-orkanoio/orkano}"
RELEASES="https://github.com/${ORKANO_REPO}/releases"

log()  { printf '  %s\n' "$*"; }
fail() { printf 'error: %s\n' "$*" >&2; exit 1; }

# Root is required (k3s install + AppArmor profile load). Do the unprivileged
# download+verify as the current user, then use sudo only for the install + init,
# so a piped `curl | sh` works without self-re-exec (no script file to re-run).
SUDO=""
if [ "$(id -u)" -ne 0 ]; then
	command -v sudo >/dev/null 2>&1 || fail "must run as root, or install sudo"
	SUDO="sudo"
fi

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
[ "$os" = "linux" ] || fail "Orkano installs on Linux only (got $os)"
case "$(uname -m)" in
	x86_64 | amd64) arch="amd64" ;;
	aarch64 | arm64) arch="arm64" ;;
	*) fail "unsupported architecture $(uname -m) — Orkano supports amd64 and arm64" ;;
esac

command -v curl >/dev/null 2>&1 || fail "curl is required"

if [ "$ORKANO_VERSION" = "latest" ]; then
	dl="${RELEASES}/latest/download"
else
	dl="${RELEASES}/download/${ORKANO_VERSION}"
fi
bin="orkano_${os}_${arch}"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

log "downloading ${bin} (${ORKANO_VERSION})"
curl -fsSL "${dl}/${bin}" -o "$tmp/orkano" || fail "download failed: ${dl}/${bin}"
curl -fsSL "${dl}/checksums.txt" -o "$tmp/checksums" || fail "download failed: ${dl}/checksums.txt"

# Verify the checksum, fail-closed: a mismatch or a missing entry refuses to run.
# Exact filename match (checksums.txt is "<hash>  <filename>"), not a suffix grep.
expected="$(awk -v f="$bin" '$2 == f {print $1}' "$tmp/checksums")"
[ -n "$expected" ] || fail "no checksum for ${bin} in checksums.txt"
if command -v sha256sum >/dev/null 2>&1; then
	actual="$(sha256sum "$tmp/orkano" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
	actual="$(shasum -a 256 "$tmp/orkano" | awk '{print $1}')"
else
	fail "need sha256sum or shasum to verify the download"
fi
[ "$actual" = "$expected" ] || fail "checksum mismatch (want $expected, got $actual) — refusing to run"
log "checksum verified"

# Verify the cosign signature over checksums.txt when cosign is available and
# the release publishes the checksums.txt.sigstore.json bundle (goreleaser
# `signs`, keyless / Sigstore). Absence of cosign degrades to checksum-only with
# a notice; a PRESENT-but-INVALID signature is fatal.
if command -v cosign >/dev/null 2>&1; then
	if curl -fsSL "${dl}/checksums.txt.sigstore.json" -o "$tmp/checksums.bundle" 2>/dev/null; then
		# The identity is pinned past the repo-name boundary to the release
		# workflow and a tag ref — a bare "^…/orkano" prefix would also accept a
		# hostile sibling repo like orkanoio/orkano-evil.
		if cosign verify-blob \
			--bundle "$tmp/checksums.bundle" \
			--certificate-identity-regexp "^https://github.com/${ORKANO_REPO}/\\.github/workflows/release\\.yml@refs/tags/v" \
			--certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
			"$tmp/checksums" >/dev/null 2>&1; then
			log "cosign signature verified"
		else
			fail "cosign signature verification failed — refusing to run"
		fi
	else
		log "no cosign signature published for this release — verified by checksum only"
	fi
else
	log "cosign not found — verified by checksum only (install cosign for signature verification)"
fi

# shellcheck disable=SC2086 # $SUDO is intentionally unquoted so an empty value drops the word.
$SUDO install -m 0755 "$tmp/orkano" /usr/local/bin/orkano
log "installed /usr/local/bin/orkano"

log "starting: orkano init --local $*"
# shellcheck disable=SC2086
exec $SUDO /usr/local/bin/orkano init --local "$@"
