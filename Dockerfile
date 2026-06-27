FROM golang:1.24-alpine AS builder
WORKDIR /app
COPY . .
RUN sed -i 's/go 1\.2[5-9]\.[0-9]*/go 1.24/g' go.mod vendor/modules.txt 2>/dev/null; \
    CGO_ENABLED=0 go build -mod=vendor -o servconsole .

FROM alpine:latest
RUN apk add --no-cache wget
WORKDIR /app
COPY --from=builder /app/servconsole .
COPY web/ ./web/
EXPOSE 8083
ENTRYPOINT ["./servconsole"]
