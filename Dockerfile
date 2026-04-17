# Build stage
FROM golang:1.25-alpine AS builder

WORKDIR /app

# Copy go mod files first for better caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the binary — pass GOPROXY as a build arg for flexibility (e.g. --build-arg GOPROXY=https://goproxy.cn,direct)
# Default builds cmd/server (echo processor). For VLM example: --build-arg BUILD_TARGET=./example/vlm/cmd
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
ARG BUILD_TARGET=./cmd/server
RUN CGO_ENABLED=0 GOOS=linux go build -o /app/server ${BUILD_TARGET}

# Runtime stage
FROM alpine:3.19

RUN apk --no-cache add ca-certificates tzdata

RUN mkdir -p /var/log/taskgate

WORKDIR /app

COPY --from=builder /app/server .
# Uncomment and adjust for business-specific assets:
# COPY example/vlm/prompts/ /app/prompts/

EXPOSE 9531

CMD ["./server"]
