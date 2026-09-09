# Build stage.
FROM golang:1.25-alpine AS build

WORKDIR /src

# Dependencies are copied first so that the module download layer is cached and
# only re-runs when go.mod/go.sum actually change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO is disabled so the binary is fully static and can run on a scratch-like
# base image. -trimpath keeps build paths out of the binary.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/gateway ./cmd/gateway

# Runtime stage: distroless, non-root, no shell and no package manager, so a
# compromised gateway has almost nothing to pivot with.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/gateway /gateway
COPY --from=build /src/tenants.json /tenants.json

USER nonroot:nonroot

# 8080 proxies traffic, 9090 serves metrics. Keeping them separate means the
# metrics port can be firewalled off without touching the data path.
EXPOSE 8080 9090

ENTRYPOINT ["/gateway"]
