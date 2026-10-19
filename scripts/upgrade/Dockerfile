# Builds the server from whatever source tree is the build context (the current one, or a checkout of an older release) with a version stamped in,
# so the rolling-upgrade test can run an "N-1" and an "N" side by side with distinguishable versions.
FROM golang:1.27 AS build
ARG VERSION
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/tasklane ./cmd/tasklane && mkdir -p /out/data/blobs

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/tasklane /usr/local/bin/tasklane
COPY --from=build --chown=65532:65532 /out/data /data
ENV TASKLANE_LISTEN=:8080 TASKLANE_BLOB_DIR=/data/blobs
USER nonroot
ENTRYPOINT ["/usr/local/bin/tasklane"]
CMD ["serve"]
