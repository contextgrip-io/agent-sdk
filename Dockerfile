# Stage 1: build the web UI
FROM node:22-alpine AS ui
WORKDIR /src/ui
COPY ui/package.json ui/package-lock.json* ./
RUN npm ci || npm install
COPY ui/ ./
RUN npm run build

# Stage 2: build the Go server with the UI embedded
FROM golang:1.26-alpine AS server
WORKDIR /src/server
COPY server/go.mod server/go.sum ./
RUN go mod download
COPY server/ ./
COPY --from=ui /src/ui/dist/ ./internal/webui/dist/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ai-chat ./cmd/ai-chat

# Stage 3: runtime
FROM alpine:3.21
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 aichat \
    && mkdir -p /data && chown aichat /data
COPY --from=server /out/ai-chat /usr/local/bin/ai-chat
USER aichat
ENV PORT=8080 AI_CHAT_DB_PATH=/data/ai-chat.sqlite
EXPOSE 8080
VOLUME /data
ENTRYPOINT ["ai-chat"]
