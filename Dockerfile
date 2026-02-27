FROM golang:1.25-alpine AS builder

WORKDIR /src

RUN apk add --no-cache ca-certificates git

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/nvidia-license-server-exporter ./cmd/nvidia-license-server-exporter

FROM alpine:3.22

RUN apk add --no-cache ca-certificates && adduser -D -H -u 65532 appuser

USER 65532:65532
WORKDIR /

COPY --from=builder /out/nvidia-license-server-exporter /nvidia-license-server-exporter

EXPOSE 9844

ENV LISTEN_ADDRESS=:9844

ENTRYPOINT ["/nvidia-license-server-exporter"]
