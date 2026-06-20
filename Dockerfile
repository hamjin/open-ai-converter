FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod ./
COPY go.sum ./
COPY vendor ./vendor/
COPY *.go ./
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o openai-converter .

FROM alpine:latest
RUN apk --no-cache add ca-certificates tzdata
WORKDIR /app
COPY --from=builder /app/openai-converter .
EXPOSE 9090
CMD ["./openai-converter"]
