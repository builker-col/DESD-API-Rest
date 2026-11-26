# Etapa 1: compilación estática de la API + Hot-Folder en Go (zero-cgo)
FROM golang:1.23-bookworm AS builder

WORKDIR /src
COPY go.mod ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN go mod tidy && CGO_ENABLED=0 go build -ldflags="-s -w" -o /desd-api ./cmd/api

# Etapa 2: runtime mínimo con MuPDF (mutool), Poppler (pdfimages/pdfinfo/pdftoppm) y libvips (SIMD)
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    mupdf-tools \
    poppler-utils \
    libvips-tools \
    ca-certificates \
    tzdata \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /desd-api /usr/local/bin/desd-api

EXPOSE 5000

HEALTHCHECK --interval=15s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/desd-api", "-healthcheck"]

CMD ["/usr/local/bin/desd-api"]
