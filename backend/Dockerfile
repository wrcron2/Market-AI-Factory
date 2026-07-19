FROM golang:1.24-alpine AS builder
RUN apk add --no-cache gcc musl-dev
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 GOOS=linux go build -o factory-server ./cmd/server

FROM alpine:3.19
RUN apk add --no-cache ca-certificates sqlite tzdata docker-cli docker-cli-compose git
WORKDIR /app
COPY --from=builder /app/factory-server .
EXPOSE 9080
CMD ["./factory-server"]
