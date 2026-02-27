# Use minimal Alpine Linux image
FROM dhi.io/alpine-base:3.23-alpine3.23-dev

# Install ca-certificates for HTTPS support
RUN apk add --no-cache ca-certificates

# Create flomation user and group
RUN addgroup -S flomation && adduser -S flomation -G flomation

# Copy the binary into the container
ARG BINARY_FILE
COPY ${BINARY_FILE} /usr/local/bin/flomation-launch

# Make the binary executable and change ownership to flomation user
RUN chmod +x /usr/local/bin/flomation-launch && \
    chown flomation:flomation /usr/local/bin/flomation-launch

# Switch to flomation user
USER flomation

# Expose any ports if needed (adjust as necessary)
# EXPOSE 8080

# Set the binary as entrypoint
ENTRYPOINT ["/usr/local/bin/flomation-launch"]