# syntax=docker/dockerfile:1

# The build stage runs on the builder's own platform and cross-compiles, so multi-arch
# images build without emulation.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
    -o /out/thehackerdispatch ./cmd/thehackerdispatch

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/thehackerdispatch /thehackerdispatch
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/thehackerdispatch"]
CMD ["serve"]
