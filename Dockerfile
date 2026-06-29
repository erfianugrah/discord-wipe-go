# Stage 1: build the static binary
# go.mod pins go 1.26.4; GOTOOLCHAIN=auto fetches that exact toolchain at
# build time (Docker Hub has no stable golang:1.26-alpine tag yet).
FROM golang:1.25-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN GOTOOLCHAIN=auto go mod download
COPY . .
RUN CGO_ENABLED=0 GOTOOLCHAIN=auto go build -ldflags="-s -w" -o discord-wipe ./cmd/discord-wipe/

# Stage 2: minimal runtime
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /build/discord-wipe /discord-wipe
ENTRYPOINT ["/discord-wipe"]
CMD ["run", "--watch"]
