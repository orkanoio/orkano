# Combined E2E helper image (test-only — never a release artifact, so a
# RUN-based assembly is fine; the COPY-only policy in build/Dockerfile governs
# the PRODUCT images). It runs two subcommands of the local-loop-helper:
#   github-stub  — the slice of the GitHub REST API the operator's dispatcher
#                  re-fetches commits from (operator --github-base-url -> here);
#   git-fixture  — the App's source repo over `git http-backend` smart HTTP
#                  (operator --git-base-url -> here),
# so the hermetic E2E reaches neither github.com nor api.github.com.
#
# alpine + the apk git package provide the git binary git-fixture serves
# through (alpine:3.22 is the same base hack/ci/substrate-smoke/fixture pins, so
# it is already pulled). git-daemon is REQUIRED on top: Alpine ships
# git-http-backend (the smart-HTTP CGI git-fixture runs) in that subpackage, not
# the base git package, so without it `git http-backend` is "not a git command"
# and every clone 500s — the build can never fetch its source. Build context =
# the dir run.sh assembles: the prebuilt local-loop-helper binary, this seed.sh,
# and the fixture/ tree, all at the context root.
FROM alpine:3.22
RUN apk add --no-cache git git-daemon
COPY local-loop-helper /usr/local/bin/local-loop-helper
COPY seed.sh /usr/local/bin/seed.sh
COPY fixture/ /fixture/
# Bake the bare repo at build time (fixed SHA), then hand it to the non-root
# runtime UID. chown (not just chmod) is load-bearing: git refuses to operate on
# a repo owned by a different user ("detected dubious ownership"), so a
# root-owned repo served as 65534 would fail git http-backend and rev-parse.
RUN sh /usr/local/bin/seed.sh /fixture /srv/git orkanoio/orkano-e2e \
    && chown -R 65534:65534 /srv/git
# Serve as non-root (the repo is world-readable; :8080 is unprivileged) so the
# pod admits without a runAsNonRoot fight wherever the harness schedules it.
USER 65534:65534
ENTRYPOINT ["local-loop-helper"]
