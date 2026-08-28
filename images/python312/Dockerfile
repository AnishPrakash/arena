# file: images/python312/Dockerfile
FROM golang:1.25-alpine AS boxrun
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/boxrun ./cmd/boxrun
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags "-s -w" -o /out/boxrun ./cmd/boxrun

FROM python:3.12-alpine
COPY --from=boxrun /out/boxrun /usr/local/bin/boxrun

# PYTHONHASHSEED=0 is a determinism control, not a micro-optimisation: without it CPython
# randomises string hashing per process, so set/dict iteration order — and therefore both
# output order and cache behaviour — differ between two runs of identical code.
ENV PYTHONHASHSEED=0 PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 \
    TMPDIR=/box LANG=C.UTF-8 TZ=UTC
WORKDIR /box
