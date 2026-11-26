# Microservicio Worker de Inferencia IA (`services/worker`)

Microservicio unificado en **Python 3.12** que carga los 4 modelos de calidad documental en un solo proceso, procesa imágenes desde `/shared-data` (`tmpfs`) usando un **pipeline solapado de 3 etapas**, persiste resultados en lote en **PostgreSQL 16** y limpia automáticamente los archivos temporales.

---

## Modelos cargados

Montados en modo solo lectura desde `./models:/models:ro`:

| Modelo | Archivo ONNX / OpenVINO | Resolución | Clases |
|---|---|---|---|
| `cude` | `cude/cude_v4.{onnx,xml}` | `960×960` | `con corte de informacion` / `sin corte de informacion` |
| `rode` | `rode/rode_v3.{onnx,xml}` | `960×960` | `rotado` / `no rotado` |
| `legibility` | `legibility/legibility_v2.{onnx,xml}` | `960×960` | `legible` / `no legible` |
| `tilde` | `tilde/tilde_v2.{onnx,xml}` | `640×640` | `inclinado` / `no inclinado` |

- **CPUs Intel (x86_64):** ejecuta automáticamente **OpenVINO IR** con `PERFORMANCE_HINT=LATENCY`.
- **CPUs ARM64 / Apple Silicon / AMD:** ejecuta automáticamente **ONNX Runtime 1.24.4** (`ORT_ENABLE_ALL`, soporte completo opset 22).

---

## Pipeline solapado de 3 etapas

1. **Etapa 1 — Prefetch (`WORKER_PREFETCH_THREADS=2`):** lee el JPEG de `tmpfs` una sola vez (`cv2.imread`) y genera los tensores NCHW `float32` con `cv2.dnn.blobFromImage` (C++/SIMD, sin copias `float64`). Los modelos con igual resolución (`cude`, `rode`, `legibility` → `960×960`) **comparten exactamente el mismo tensor**.
2. **Etapa 2 — Inferencia paralela:** ejecuta todos los modelos solicitados para esa página en paralelo mediante `ThreadPoolExecutor` (liberando el GIL) mientras la etapa 1 ya decodifica la página siguiente.
3. **Etapa 3 — Escritor PostgreSQL en micro-lotes:** agrupa hasta `WORKER_DB_BATCH_SIZE` páginas (ventana `WORKER_DB_FLUSH_WINDOW_MS`) en **una sola transacción** (`1 bulk INSERT` en `result_item` + `1 UPDATE` atómico en `result_status`), elimina el `.jpg` y el directorio vacío de `/shared-data`, y envía `basic_ack` thread-safe a RabbitMQ.

### Tolerancia a fallos (DLQ)
Si una imagen falta o está corrupta, el worker limpia el archivo y responde `basic_nack(requeue=False)`, enrutando el mensaje a `queue_worker_dlq` vía exchange `desd.dlx` sin detener el servicio.

---

## Variables de entorno

| Variable | Valor por defecto | Descripción |
|---|---|---|
| `RABBITMQ_HOST` | `rabbitmq` | Host de RabbitMQ |
| `RABBITMQ_USER` / `RABBITMQ_PASS` | `user` / `password` | Credenciales AMQP |
| `RABBITMQ_DLX` | `desd.dlx` | Exchange Dead-Letter |
| `RABBITMQ_PREFETCH` | `8` | Mensajes en vuelo para alimentar el prefetch |
| `QUEUE_NAME` | `queue_worker` | Cola de consumo |
| `POSTGRES_URI` | `postgresql://user:password@postgres:5432/desd` | URI de PostgreSQL |
| `MODELS_DIR` | `/models` | Carpeta raíz de modelos |
| `ENABLED_MODELS` | `cude,rode,legibility,tilde` | Modelos a cargar en memoria |
| `INFERENCE_NUM_THREADS` | `cpus // num_models` | Hilos de cómputo por modelo |
| `WORKER_PREFETCH_THREADS` | `2` | Hilos dedicados a lectura y preprocesamiento |
| `WORKER_DB_BATCH_SIZE` | `8` | Máximo de páginas por transacción PostgreSQL |
| `WORKER_DB_FLUSH_WINDOW_MS` | `15` | Ventana máxima (ms) antes de hacer commit |

---

## Escalado horizontal

```bash
docker compose up -d --scale worker=2
```
Cada réplica consume de `queue_worker` en balanceo automático; la actualización atómica de `total_items_processed` en PostgreSQL y la limpieza con `os.rmdir` son seguras ante concurrencia entre réplicas.
