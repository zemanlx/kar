# syntax=docker/dockerfile:1

FROM golang:1.27.2 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/kar .

FROM scratch
COPY --from=build /out/kar /kar
USER 65532:65532
ENTRYPOINT ["/kar"]
