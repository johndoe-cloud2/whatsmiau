FROM golang:1.25-alpine AS builder

WORKDIR /app

# Install gcc and SQLite dev libraries
RUN apk add build-base sqlite-dev gcc musl-dev

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Enable CGO
RUN CGO_ENABLED=1 GOOS=linux go build -a -installsuffix cgo -o whatsmiau main.go

FROM alpine:latest

RUN apk update && apk add --no-cache ffmpeg mailcap jq

WORKDIR /app

COPY --from=builder /app/whatsmiau /app/whatsmiau
COPY scripts/entrypoint-ecs.sh /app/entrypoint-ecs.sh
RUN chmod +x /app/entrypoint-ecs.sh && mkdir -p /app/data && chmod 777 /app/data

EXPOSE 8080
EXPOSE 8081

# Use entrypoint-ecs.sh when BACKEND_PUBLIC_URL is not set (e.g. ECS); it sets it from metadata and runs whatsmiau.
# To skip and run whatsmiau directly, set BACKEND_PUBLIC_URL in env or use: docker run ... ./whatsmiau
ENTRYPOINT ["/app/entrypoint-ecs.sh"]