# syntax=docker/dockerfile:1
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/tasklane ./cmd/tasklane

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/tasklane /usr/local/bin/tasklane
ENV TASKLANE_LISTEN=:8080 TASKLANE_BLOB_DIR=/data/blobs
VOLUME /data
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/usr/local/bin/tasklane"]
CMD ["serve"]
