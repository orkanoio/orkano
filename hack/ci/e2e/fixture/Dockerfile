# The user app the hermetic M1.6 E2E builds end to end. A tiny web server on
# :8080 (the App's default web port, so the Web archetype needs zero port
# config) that returns a known body, so the E2E can assert the rolled
# Deployment answers HTTP on the digest-pinned image it just built. Runs
# non-root so the App pod admits cleanly under orkano-apps baseline PSA (and
# would not even warn at restricted).
#
# FROM a Docker Hub base by design: the rootless build pod pulls it mid-build
# over the orkano-builds egress allowlist's tcp/443 rule, exercising that leg
# (mirrors hack/ci/substrate-smoke/fixture). The git repo this Dockerfile lives
# in is served in-cluster by the git fixture, never github.com.
#
# Tag-pinned (not digest-pinned), matching the substrate-smoke fixture's
# `FROM alpine:3.22`: the build pod's BuildKit has its own image store, so a
# node-side digest pin / kind-load would not cover this mid-build pull anyway;
# the anonymous-pull flake is mitigated the same way the substrate smoke
# documents (a docker-login step, added only if the flake becomes real).
FROM busybox:1.37
RUN mkdir -p /www && echo "orkano-e2e-ok" > /www/index.html
USER 65534:65534
EXPOSE 8080
ENTRYPOINT ["httpd", "-f", "-v", "-p", "8080", "-h", "/www"]
