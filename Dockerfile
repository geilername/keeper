# One image recipe for every Go service: docker build --build-arg SERVICE=ingest .
FROM golang:1.27.3-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG SERVICE
ARG VERSION=dev
# nodynamic: the WebP encoder must not try to load a system libwebp at run
# time; that makes the binary dynamically linked, and distroless has no loader.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -tags nodynamic \
      -ldflags "-s -w -X github.com/geilername/keeper/internal/telemetry.Version=${VERSION}" \
      -o /out/app ./cmd/${SERVICE}

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
USER 1000:1000
ENTRYPOINT ["/app"]
