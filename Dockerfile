# Stage 1: build the static binary
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
