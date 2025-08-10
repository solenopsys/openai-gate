# ---------- build ----------
    FROM golang:1.24.2-alpine AS builder

    WORKDIR /app
    RUN apk add --no-cache git ca-certificates tzdata gcc musl-dev
    
    # кешируем зависимости
    COPY go.mod go.sum* ./
    RUN go mod download
    
    # копируем исходники
    COPY . .
    
    # собираем бинарь
    RUN CGO_ENABLED=1 \
        GOOS=linux \
        go build -ldflags="-s -w" -o /sip-webrtc-openai ./cmd/sip-webrtc-openai
    
    # ---------- runtime ----------
    FROM alpine:latest
    
    WORKDIR /app
    RUN apk add --no-cache ca-certificates tzdata sqlite
    
    COPY --from=builder /sip-webrtc-openai .
    COPY index.html .
    
    USER 1000 
    ENTRYPOINT ["./sip-webrtc-openai"]
    