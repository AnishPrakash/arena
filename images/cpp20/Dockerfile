# file: images/cpp20/Dockerfile
# Build context is the repo root: docker build -f images/cpp20/Dockerfile .

FROM golang:1.25-alpine AS boxrun
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/boxrun ./cmd/boxrun
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags "-s -w" -o /out/boxrun ./cmd/boxrun

FROM alpine:3.19
# Alpine + musl keeps the image ~200 MB instead of gcc:13's ~1.2 GB. Smaller images pull
# faster on node startup, which is real money when runners scale up and down all day.
RUN apk add --no-cache g++ musl-dev libstdc++
COPY --from=boxrun /out/boxrun /usr/local/bin/boxrun

# TMPDIR must point inside the writable box: the rootfs is mounted read-only, and g++
# writes temporary objects. (-pipe in the compile command avoids most of them anyway.)
ENV TMPDIR=/box LANG=C.UTF-8 TZ=UTC
WORKDIR /box

# uid 65534 (nobody) already exists in alpine. The container runs as it via --user, so
# nothing in the image needs to be world-writable.
