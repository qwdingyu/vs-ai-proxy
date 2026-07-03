FROM golang:1.22-alpine AS build

WORKDIR /src

RUN apk add --no-cache ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/vs-ai-proxy ./cmd/server

FROM alpine:3.20

RUN apk add --no-cache ca-certificates \
	&& addgroup -S app \
	&& adduser -S app -G app \
	&& mkdir -p /data \
	&& chown -R app:app /data

WORKDIR /app

COPY --from=build /out/vs-ai-proxy /usr/local/bin/vs-ai-proxy

ENV CONFIG_PATH=/data/config.json
ENV HOST=0.0.0.0

EXPOSE 12345

USER app

ENTRYPOINT ["vs-ai-proxy"]
