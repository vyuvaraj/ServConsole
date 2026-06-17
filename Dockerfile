# Step 1: Build the Go backend binary
FROM golang:1.21-alpine AS builder
WORKDIR /app
COPY go.mod ./
COPY main.go ./
RUN go build -o servconsole main.go

# Step 2: Assemble the production image
FROM alpine:latest
WORKDIR /app
COPY --from=builder /app/servconsole .
COPY web/ ./web/
EXPOSE 8083
ENTRYPOINT ["./servconsole"]
