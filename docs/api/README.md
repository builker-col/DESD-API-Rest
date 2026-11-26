# Referencia de la API REST v1 — DESD-API-Rest

Documentación técnica completa de todos los endpoints expuestos por el servicio `api` (Go 1.23, puerto interno `5000`).

---

## Seguridad y acceso

### 1. Control de acceso por IP / CIDR
Todas las rutas (excepto `GET /health` y `OPTIONS`) validan la IP del cliente contra la tabla `allowed_ips` en PostgreSQL (con caché en memoria de 15s e invalidación inmediata al modificarla vía `/api/v1/admin/allowed_ips`).
- Soporta IPs exactas (`127.0.0.1`, `::1`) y subredes CIDR (`172.16.0.0/12`, `192.168.0.0/16`).
- Si hay proxy inverso, toma la primera IP de la cabecera `X-Forwarded-For`.
- Si la IP no está autorizada, responde `403 Forbidden`.

### 2. Autenticación JWT (HS256)
- Obtener token: `POST /api/v1/auth` con `{ "username": "...", "password": "..." }`.
- Vigencia del token: **24 horas** (`sub` = ID numérico del usuario en string).
- Uso en rutas protegidas: cabecera `Authorization: Bearer <access_token>`.

### 3. Códigos de estado HTTP y formato de error
Todos los errores responden JSON consistente: `{ "message": "<descripción>" }`.

| Código | Significado |
|---|---|
| `200 OK` | Solicitud exitosa (JSON o stream CSV) |
| `201 Created` | Recurso creado |
| `202 Accepted` | Lote aceptado y encolado para procesamiento asíncrono |
| `204 No Content` | Recurso eliminado |
| `400 Bad Request` | Parámetros inválidos o faltantes |
| `401 Unauthorized` | Token ausente, expirado o credenciales inválidas |
| `403 Forbidden` | IP no permitida o permisos de administrador requeridos |
| `404 Not Found` | Recurso o `task_id` no encontrado |
| `409 Conflict` | `task_id` ya existe |
| `503 Service Unavailable` | Dependencia degradada en `/health` |

---

## Resumen de endpoints

| Método | Ruta | Auth | Descripción |
|---|---|---|---|
| `GET` | `/health` | No (sin filtro IP) | Estado de salud de `api`, PostgreSQL y RabbitMQ |
| `POST` | `/api/v1/auth` | No | Autenticación y emisión de JWT |
| `POST` | `/api/v1/user` | No | Registro de nuevo usuario (no admin) |
| `GET` / `PUT` / `DELETE` | `/api/v1/user` | JWT | Consultar, actualizar o eliminar el usuario autenticado |
| `POST` | `/api/v1/worker` | JWT | Sube archivos y encola inferencia multi-modelo |
| `POST` | `/api/v1/duplicate` | JWT | Sube documentos y encola detección de páginas duplicadas |
| `GET` | `/api/v1/task` | JWT | Lista tareas del usuario autenticado (con filtros y orden) |
| `GET` / `DELETE` | `/api/v1/task/{task_id}` | JWT | Consulta detalle o elimina una tarea (y sus resultados) |
| `GET` | `/api/v1/export/{task_id}` | JWT | Exporta resultados en streaming CSV (`normal` \| `no_tecnico`) |
| `GET` / `POST` / `PUT` / `DELETE` | `/api/v1/admin/user[/{user_id}]` | JWT (Admin) | Administración completa de usuarios |
| `GET` / `DELETE` | `/api/v1/admin/task[/{task_id}]` | JWT (Admin) | Administración global de tareas |
| `GET` / `POST` / `PUT` / `DELETE` | `/api/v1/admin/allowed_ips[/{id}]` | JWT (Admin) | Gestión de IPs y rangos CIDR permitidos |

---

## Especificación de endpoints

### Healthcheck

#### `GET /health`
Verifica conectividad activa contra PostgreSQL y RabbitMQ.
```json
{
  "status": "ok",
  "postgres": true,
  "rabbitmq": true
}
```

---

### Autenticación

#### `POST /api/v1/auth`
- **Body JSON:** `{ "username": "admin", "password": "admin" }`
- **Respuesta `200 OK`:**
```json
{
  "access_token": "eyJhbGciOiJIUzI1NiIs..."
}
```

---

### Procesamiento de documentos

#### `POST /api/v1/worker`
Convierte documentos en paralelo directo a `/shared-data` (`tmpfs`) y publica en streaming a `queue_worker`.
- **Content-Type:** `multipart/form-data`
- **Campos:**
  - `files` *(requerido, repetible)*: imágenes (`jpg`, `jpeg`, `png`), documentos (`pdf`, `tif`, `tiff`) o un `.zip`.
  - `models` *(requerido)*: lista CSV de modelos válidos `{cude, rode, tilde, legibility}`.
  - `task_id` *(opcional)*: identificador único; si se omite se genera automáticamente.
- **Respuesta `202 Accepted`:**
```json
{
  "task_id": "lote_2026_01"
}
```
- **Ejemplo:**
```bash
curl -X POST http://localhost:5000/api/v1/worker \
  -H "Authorization: Bearer $TOKEN" \
  -F "models=cude,rode,legibility,tilde" \
  -F "task_id=lote_2026_01" \
  -F "files=@factura_1.pdf" \
  -F "files=@escaneo_2.tiff"
```

#### `POST /api/v1/duplicate`
Convierte documentos PDF/TIFF y encola detección de páginas duplicadas (`queue_dude`).
- **Content-Type:** `multipart/form-data`
- **Campos:**
  - `files` *(requerido, repetible)*: documentos (`pdf`, `tif`, `tiff`) o un `.zip`.
  - `task_id` *(opcional)*: identificador único.
- **Respuesta `202 Accepted`:** `{ "task_id": "..." }`

---

### Seguimiento y gestión de tareas

#### `GET /api/v1/task`
Lista las tareas del usuario autenticado.
- **Query params opcionales:**
  - `status`: `UPLOADING` | `PENDING` | `RUNNING` | `COMPLETED` | `FAILED`
  - `order_type`: `created_at` *(defecto)* | `updated_at` | `id` | `collection_id`
  - `order_by`: `desc` *(defecto)* | `asc`
- **Respuesta `200 OK`:**
```json
{
  "total_results": 1,
  "results": [
    {
      "id": 12,
      "collection_id": "lote_2026_01",
      "user_id": 1,
      "created_at": "2026-11-26T00:10:00Z",
      "updated_at": "2026-11-26T00:10:00Z",
      "status": {
        "id": 12,
        "result_id": 12,
        "status": "COMPLETED",
        "total_files": 2,
        "total_files_processed": 2,
        "total_items_expected": 80,
        "total_items_processed": 80,
        "models": "cude,rode,legibility,tilde",
        "current_file": "escaneo_2.tiff",
        "last_updated": "2026-11-26T00:10:01Z"
      }
    }
  ]
}
```

#### `GET /api/v1/task/{task_id}`
Devuelve el objeto de tarea individual con su estado detallado.

#### `DELETE /api/v1/task/{task_id}`
Elimina la tarea, su estado, todos los registros asociados en `result_item` y cualquier archivo residual en `/shared-data/{task_id}` (requiere ser el propietario; devuelve `204 No Content`).

---

### Exportación CSV en streaming

#### `GET /api/v1/export/{task_id}`
Transmite los resultados directo desde un cursor `pgx` de PostgreSQL hacia la respuesta HTTP (`Content-Type: text/csv; charset=utf-8`), sin archivos temporales y con uso constante de memoria.

- **Query params:**
  - `formato` *(opcional)*: `normal` *(defecto)* | `no_tecnico`

#### Comportamiento de formatos
- **`normal`:**
  - Para inferencia (`worker`): columnas `task_id,nombre_archivo,pagina,confianza,nombre_clase,modelo,tiempo_inferencia`.
  - Para duplicados (`dude`): columnas `task_id,filename,pagina1,pagina2,similitud` (y `message` si algún documento no tuvo duplicados).
- **`no_tecnico`:**
  - Filtra únicamente hallazgos problemáticos con `nombre_clase` en: `inclinado`, `rotado`, `con corte de informacion`, `no legible`.
  - Requiere `confianza >= 80%` (acepta formato `"97.33%"` o numérico).
  - Convierte la confianza a etiqueta legible:
    - `>= 95%` → `muy probable`
    - `90% – 94.99%` → `probable`
    - `80% – 89.99%` → `quizás con error`
  - Columnas fijas en orden: `task_id,nombre_archivo,pagina,nombre_clase,confianza`.

```bash
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:5000/api/v1/export/lote_2026_01?formato=no_tecnico" -o reporte.csv
```

---

### Administración (requiere `is_admin = true`)

- **Usuarios:** `GET /api/v1/admin/user`, `GET /api/v1/admin/user/{user_id}`, `POST /api/v1/admin/user`, `PUT /api/v1/admin/user/{user_id}`, `DELETE /api/v1/admin/user/{user_id}`
- **Tareas globales:** `GET /api/v1/admin/task`, `GET /api/v1/admin/task/{task_id}`, `DELETE /api/v1/admin/task/{task_id}`
- **Lista blanca de IPs:** `GET /api/v1/admin/allowed_ips`, `GET /api/v1/admin/allowed_ips/{id}`, `POST /api/v1/admin/allowed_ips`, `PUT /api/v1/admin/allowed_ips/{id}`, `DELETE /api/v1/admin/allowed_ips/{id}`

---

## Diagramas

- [docs/api/endpoints-overview.mmd](endpoints-overview.mmd)
- [docs/api/auth-flow.mmd](auth-flow.mmd)
- [docs/api/er-model.mmd](er-model.mmd)
- [docs/api/sequence-export.mmd](sequence-export.mmd)
