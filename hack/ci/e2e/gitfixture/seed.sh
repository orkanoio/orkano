#!/usr/bin/env sh
# Seed a deterministic bare git repo from a working tree, for the hermetic M1.6
# E2E git fixture. Run at image-build time (gitfixture/Dockerfile) so the HEAD
# SHA is fixed before the cluster exists; run.sh discovers it at runtime via
# `git rev-parse` and feeds it to the github-stub, so the commit the dispatcher
# "resolves" is exactly the one BuildKit checks out.
#
#   seed.sh <source-worktree> <project-root> <owner/name>
#
# Fixed identity + dates make the commit reproducible across builds. The bare
# repo gets uploadpack.allowAnySHA1InWant so BuildKit can fetch the resolved
# commit directly (it fetches by SHA, not by ref). Prints the HEAD SHA on stdout.
set -eu

SRC="$1"        # working tree to commit (the fixture app source)
ROOT="$2"       # GIT_PROJECT_ROOT the server exports (e.g. /srv/git)
REPO="$3"       # owner/name the repo is served as (e.g. orkanoio/orkano-e2e)

export GIT_AUTHOR_NAME="Orkano E2E" GIT_AUTHOR_EMAIL="e2e@orkano.invalid" GIT_AUTHOR_DATE="2026-01-01T00:00:00Z"
export GIT_COMMITTER_NAME="Orkano E2E" GIT_COMMITTER_EMAIL="e2e@orkano.invalid" GIT_COMMITTER_DATE="2026-01-01T00:00:00Z"
# Hermetic: ignore any host/user git config (hooks, identity, protocol limits).
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null

work="$(mktemp -d)"
cp -a "$SRC"/. "$work"/
git -C "$work" init -q -b main
git -C "$work" add -A
git -C "$work" commit -q -m "orkano e2e fixture"

bare="$ROOT/$REPO.git"
mkdir -p "$(dirname "$bare")"
rm -rf "$bare"
git clone --bare -q "$work" "$bare"
git -C "$bare" config uploadpack.allowAnySHA1InWant true
rm -rf "$work"

git -C "$bare" rev-parse HEAD
