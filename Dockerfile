FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
# Static, so the final image needs no libc and the Docker socket is the only
# thing this container touches.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/docker-health-exporter ./cmd/docker-health-exporter

FROM alpine:3.21
RUN apk add --no-cache wget
COPY --from=build /out/docker-health-exporter /usr/local/bin/docker-health-exporter
# UID 65534 is already taken by `nobody` on alpine:3.21, so there is no
# `exporter` user to add; run as nobody directly.
USER nobody:nogroup
EXPOSE 9415
ENTRYPOINT ["/usr/local/bin/docker-health-exporter"]
