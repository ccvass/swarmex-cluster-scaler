FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /swarmex-cluster-scaler ./cmd

FROM alpine:3.21
RUN apk add --no-cache aws-cli python3
COPY --from=build /swarmex-cluster-scaler /usr/local/bin/swarmex-cluster-scaler
HEALTHCHECK --interval=10s --timeout=3s CMD wget -qO- http://localhost:8080/health || exit 1
ENTRYPOINT ["swarmex-cluster-scaler"]
