# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/promptape ./cmd/promptape

# distroless/static ships CA certificates (needed to reach the real APIs)
# and runs as an unprivileged user.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/promptape /usr/local/bin/promptape
ENV PROMPTAPE_HOST=0.0.0.0 \
    PROMPTAPE_PORT=4000 \
    PROMPTAPE_DIR=/tapes
WORKDIR /tapes
VOLUME ["/tapes"]
EXPOSE 4000
ENTRYPOINT ["/usr/local/bin/promptape"]
CMD ["serve"]
