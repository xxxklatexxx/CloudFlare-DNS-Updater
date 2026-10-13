# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY *.go dashboard.html ./
RUN go test ./...
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/ddns .

FROM build AS race-test
RUN apk add --no-cache build-base
RUN CGO_ENABLED=1 go test -race ./...

FROM alpine:3.23 AS certificates
RUN apk add --no-cache ca-certificates && mkdir -p /state && chown 65532:65532 /state

FROM scratch
COPY --from=certificates /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=certificates --chown=65532:65532 /state /state
COPY --from=build /out/ddns /ddns
USER 65532:65532
EXPOSE 8080
VOLUME ["/state"]
HEALTHCHECK --interval=30s --timeout=6s --start-period=90s --retries=3 CMD ["/ddns", "-healthcheck"]
ENTRYPOINT ["/ddns"]
