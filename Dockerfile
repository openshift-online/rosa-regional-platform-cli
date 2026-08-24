# Build stage
FROM registry.access.redhat.com/ubi9/go-toolset:1787080752 AS builder

# Switch to root to set up workspace
USER root

WORKDIR /build

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o rosactl ./cmd/rosactl

# Runtime stage - UBI minimal for smaller image size
FROM registry.access.redhat.com/ubi9/ubi-minimal:latest

# Install AWS Lambda Runtime Interface Emulator (for local testing)
# and ca-certificates for TLS connections
RUN microdnf install -y ca-certificates tar gzip && microdnf clean all

# Copy the Go binary
COPY --from=builder /build/rosactl /usr/local/bin/rosactl

# Create non-root user
RUN useradd -u 1001 -r -g 0 -m -d /app -s /sbin/nologin \
    -c "Lambda user" lambda

# Set ownership
RUN chown -R 1001:0 /app && chmod -R g=u /app

# Switch to non-root user
USER 1001

# Set working directory
WORKDIR /app

# Lambda handler entrypoint
# When Lambda invokes the container, it will call this command
ENTRYPOINT ["/usr/local/bin/rosactl"]
CMD ["handler"]
