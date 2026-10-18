# syntax=docker/dockerfile:1
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/tasklane ./cmd/tasklane && mkdir -p /out/data/blobs

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/tasklane /usr/local/bin/tasklane
# a fresh named volume inherits this ownership, so the non-root user can write the blob store
COPY --from=build --chown=65532:65532 /out/data /data
ENV TASKLANE_LISTEN=:8080 TASKLANE_BLOB_DIR=/data/blobs
VOLUME /data
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/usr/local/bin/tasklane"]
CMD ["serve"]
