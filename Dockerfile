# Builds Playlister for macOS, Linux and Windows – no local Go installation needed.
#
#   docker build --output dist .
#   docker build --build-arg VERSION=v1.0.0 --output dist .
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY web ./web
ARG VERSION=dev
ENV CGO_ENABLED=0
RUN go vet ./... && go test ./...
RUN set -e; \
    for t in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64; do \
      os=${t%/*}; arch=${t#*/}; ext=""; [ "$os" = windows ] && ext=".exe"; \
      name=$os; [ "$os" = darwin ] && name=macos; \
      echo "-> $name-$arch"; \
      GOOS=$os GOARCH=$arch go build -trimpath \
        -ldflags="-s -w -X main.version=$VERSION" \
        -o /out/playlister-$name-$arch$ext . ; \
    done

FROM scratch
COPY --from=build /out/ /
