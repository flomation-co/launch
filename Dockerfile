# Use minimal Alpine Linux image
FROM dhi.io/alpine-base:3.23-alpine3.23-dev

# Install ca-certificates for HTTPS support
RUN apk add --no-cache ca-certificates

# Create the flomation user and group, pinned to an explicit uid/gid.
# Pinning matters: `adduser -S` with no -u takes the first free system id, which
# collides with package-provided accounts — that is how the runner image ended
# up on uid 101, because `apk add clamav` claimed 100 first. 10001 is outside
# the range apk allocates from and is free in every base image we use.
RUN addgroup -g 10001 -S flomation && \
    adduser  -u 10001 -S flomation -G flomation

# Copy the binary into the container.
# Owned by root and mode 0555: the application cannot rewrite its own
# executable, and one COPY replaces COPY + chmod + chown, which previously
# rewrote every byte of the binary into a second image layer.
ARG BINARY_FILE
COPY --chown=root:root --chmod=0555 ${BINARY_FILE} /usr/local/bin/flomation-launch

# Numeric rather than a name: with `runAsNonRoot: true` the kubelet refuses an
# image whose USER is a name, because it cannot verify the name is not root.
# The account is still called `flomation`, so `ps` and `ls -l` stay readable.
USER 10001:10001

# Expose any ports if needed (adjust as necessary)
# EXPOSE 8080

# Set the binary as entrypoint
ENTRYPOINT ["/usr/local/bin/flomation-launch"]