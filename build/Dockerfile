# Shared product-image Dockerfile: COPY-only by policy (supply-chain.md) —
# no RUN steps, ever, so multi-arch assembly never needs emulation.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:d093aa3e30dbadd3efe1310db061a14da60299baff8450a17fe0ccc514a16639
ARG BINARY
COPY ${BINARY} /app
USER 65532:65532
ENTRYPOINT ["/app"]
