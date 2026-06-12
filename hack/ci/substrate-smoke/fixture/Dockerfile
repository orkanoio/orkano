# Build fixture for the substrate smoke. FROM a Docker Hub base on purpose:
# the pull exercises the tcp/443 egress allowlist leg the way real builds will.
FROM alpine:3.22
RUN echo "built by rootless buildkit from a git-URL context" > /hello
CMD ["cat", "/hello"]
