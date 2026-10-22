# Cross-compiles on the build host (fast even for arm64), ships a ~5 MB static binary.
FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY web ./web
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/disk-live-usage .

FROM scratch
COPY --from=build /out/disk-live-usage /disk-live-usage
EXPOSE 8080
# Root is required to read /proc/<pid>/io of other users' processes.
USER 0:0
ENTRYPOINT ["/disk-live-usage"]
