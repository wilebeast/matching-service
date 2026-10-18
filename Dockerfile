FROM golang:1.26-alpine AS builder
WORKDIR /src
ENV GOPROXY=https://goproxy.cn,direct
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/matching-service ./cmd/server
FROM alpine:3.22
RUN addgroup -g 10001 -S app && adduser -u 10001 -S -G app app && mkdir /data && chown app:app /data
WORKDIR /app
COPY --from=builder /out/matching-service /app/matching-service
COPY examples /app/examples
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/app/matching-service"]
