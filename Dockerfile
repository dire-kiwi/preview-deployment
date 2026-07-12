# syntax=docker/dockerfile:1
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/orchestrator ./cmd/orchestrator

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=build /out/orchestrator /usr/local/bin/orchestrator
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/orchestrator"]
