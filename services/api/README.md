# Microservicio API, Conversor y Hot-Folder (`services/api`)

Binario estático compilado en **Go 1.23** (`CGO_ENABLED=0`) que concentra en un solo servicio ligero (~13 MiB de RAM en operación):

1. **API REST v1** (`net/http` + JWT HS256 + `bcrypt` + caché CIDR de `allowed_ips`).
2. **Motor de conversión ultrarrápido** (`pdfimages -j`, `mutool draw` paralelo por bloques a `960px`, `libvips` SIMD para Multi-TIFF e imágenes grandes).
3. **Ingesta en streaming** hacia `/shared-data` (`tmpfs`) y RabbitMQ (`queue_worker` / `queue_dude` con DLQ).
4. **Exportación CSV en streaming** directo desde cursor `pgx` de PostgreSQL (`O(1)` en memoria).
5. **Hot-Folder Watcher integrado** (opcional vía `HOTFOLDER_ENABLED=true`).

---

## Estructura de paquetes

```text
services/api/
├── Dockerfile               # Build multi-stage (golang:1.23 -> debian:bookworm-slim)
├── go.mod
├── cmd/
│   └── api/
│       └── main.go          # Arranque HTTP, señalización, hot-folder y flag -healthcheck
└── internal/
    ├── config/              # Carga y validación de variables de entorno
    ├── db/                  # Pool pgx/v5, migración/seed inicial, CRUD y caché CIDR
    ├── queue/               # Publicador AMQP persistente + declaración DLX/DLQ
    ├── converter/           # pdfimages -j, mutool paralelo, pdftoppm fallback, libvips
    ├── ingest/              # Pool de goroutines para PDF, TIFF, JPG/PNG y ZIP en streaming
    ├── export/              # Streaming CSV (normal y no_tecnico) a HTTP o archivo atómico
    ├── hotfolder/           # Watcher de trigger.txt / trigger.json y gestión de lotes
    └── httpapi/             # Handlers REST v1, middleware CORS/IP/JWT y GET /health
```

---

## Variables de entorno

| Variable | Valor por defecto | Descripción |
|---|---|---|
| `API_LISTEN_ADDR` | `:5000` | Dirección y puerto HTTP |
| `POSTGRES_URI` | `postgresql://user:password@postgres:5432/desd` | URI de PostgreSQL |
| `RABBITMQ_HOST` | `rabbitmq` | Host de RabbitMQ |
| `RABBITMQ_USER` / `RABBITMQ_PASS` | `user` / `password` | Credenciales AMQP |
| `RABBITMQ_DLX` | `desd.dlx` | Exchange Dead-Letter |
| `JWT_SECRET_KEY` | `jwt_secret_key` | Clave de firma JWT HS256 (**cambiar en producción**) |
| `CONVERTER_MAX_DIM` | `960` | Lado máximo en píxeles para `mutool`, `pdftoppm` y `libvips` |
| `CONVERTER_DPI` | `150` | DPI de respaldo cuando `CONVERTER_MAX_DIM=0` |
| `CONVERTER_JPEG_QUALITY` | `85` | Calidad JPEG de salida |
| `CONVERTER_FAST_PDFIMAGES` | `1` | Activa extracción sin rasterizado para PDFs escaneados JPEG |
| `CONVERTER_PDF_CHUNK_SIZE` | `8` | Páginas por bloque paralelo en `mutool` |
| `INGEST_CONCURRENCY` | `4` | Goroutines simultáneas de conversión por lote |
| `SHARED_DATA_DIR` | `/shared-data` | Volumen compartido (`tmpfs`) |
| `HOTFOLDER_ENABLED` | `true` | Habilita el watcher local dentro del mismo proceso |
| `HOTFOLDER_ROOT` | `/hotfolder` | Carpeta raíz del hot-folder |
| `HOTFOLDER_POLL_INTERVAL` | `0.5` | Intervalo de sondeo en segundos |
| `HOTFOLDER_DEFAULT_USER` | `admin` | Usuario propietario por defecto para lotes del hot-folder |

---

## Healthcheck integrado

El binario incluye modo healthcheck sin depender de `curl`:
```bash
/usr/local/bin/desd-api -healthcheck
```
Consulta `GET http://127.0.0.1:5000/health` (verifica PostgreSQL y RabbitMQ) y retorna código `0` si todo está sano o `1` en caso contrario.

---

## Referencia de endpoints

Consulta la documentación completa de endpoints, payloads y ejemplos `curl` en [docs/api/README.md](../../docs/api/README.md).
