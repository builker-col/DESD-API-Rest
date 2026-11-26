# Microservicio de Detección de Páginas Duplicadas (`services/duplicate`)

Microservicio en **Python 3.12** que consume tareas desde RabbitMQ (`queue_dude`) y detecta páginas duplicadas dentro de documentos multipágina (`PDF`, `TIFF`/`Multi-TIFF`), guardando los hallazgos en **PostgreSQL 16** (`result_item` JSONB) y eliminando las imágenes temporales de `/shared-data` (`tmpfs`) al terminar.

---

## Algoritmo optimizado en 2 fases

1. **Filtrado rápido por hash perceptual (pHash en paralelo):**
   - Calcula el `pHash` (`32×32`) de todas las páginas del documento concurrentemente con `ThreadPoolExecutor`.
   - Solo los pares `(i, j)` con distancia de Hamming `<= 5` pasan a la fase de verificación estructural, evitando comparaciones innecesarias `O(N²)`.
2. **Verificación estructural SSIM con caché en memoria y escalado `512px`:**
   - Carga en escala de grises **una sola vez** las páginas candidatas, redimensionadas a un lado máximo de `DUPLICATE_SSIM_MAX_DIM` (`512px`, `cv2.INTER_AREA`).
   - Evalúa `structural_similarity (SSIM)` en paralelo; los pares con similitud `> 0.95` se registran en bloque en una sola transacción en PostgreSQL.

---

## Formato de resultados en PostgreSQL (`result_item.data`)

- **Cuando se encuentran duplicados:**
```json
{
  "task_id": "lote_dup_01",
  "filename": "contrato.pdf",
  "pagina1": 1,
  "pagina2": 4,
  "similitud": 0.9942
}
```
- **Cuando un documento no tiene páginas duplicadas:**
```json
{
  "task_id": "lote_dup_01",
  "filename": "anexo.pdf",
  "message": "No se encontraron duplicados."
}
```

---

## Variables de entorno

| Variable | Valor por defecto | Descripción |
|---|---|---|
| `RABBITMQ_HOST` | `rabbitmq` | Host de RabbitMQ |
| `RABBITMQ_USER` / `RABBITMQ_PASS` | `user` / `password` | Credenciales AMQP |
| `RABBITMQ_DLX` | `desd.dlx` | Exchange Dead-Letter (`queue_dude_dlq`) |
| `QUEUE_NAME` | `queue_dude` | Cola de consumo |
| `POSTGRES_URI` | `postgresql://user:password@postgres:5432/desd` | URI de PostgreSQL |
| `DUPLICATE_SSIM_MAX_DIM` | `512` | Lado máximo en píxeles antes de calcular SSIM (`0` desactiva el escalado) |

---

## Manejo de errores y DLQ

Ante cualquier fallo durante el procesamiento de un lote, el servicio actualiza `result_status.status = 'FAILED'` en PostgreSQL y envía `basic_nack(requeue=False)`, enrutando el mensaje a `queue_dude_dlq` para auditoría sin interrumpir el servicio.
