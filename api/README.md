## Introducción

API REST para procesamiento de documentos e imágenes con colas de trabajo y exportación de resultados. Autenticación basada en JWT. Persistencia de estados en PostgreSQL y resultados en MongoDB.

## Requisitos

- Python 3.10+
- PostgreSQL 13+
- MongoDB 6+
- RabbitMQ

## Autenticación (JWT)

- Endpoint de login: `POST /api/v1/auth`
- Cuerpo: `{ "username": "user", "password": "pass" }`
- Respuesta: `{ "access_token": "<JWT>" }`
- Usa el token en `Authorization: Bearer <JWT>` para los endpoints protegidos.

## Errores comunes

- 400: Datos inválidos o faltantes
- 401: No autenticado / credenciales inválidas
- 403: Sin permisos
- 404: Recurso no encontrado
- 409: Conflicto (p.ej. `task_id` duplicado)

## Endpoints

| Método | Ruta | Auth | Descripción |
|---|---|---|---|
| POST | `/api/v1/auth` | No | Obtiene JWT |
| POST | `/api/v1/user` | No | Crea usuario |
| GET | `/api/v1/user` | Sí | Obtiene usuario actual |
| PUT | `/api/v1/user` | Sí | Actualiza usuario actual |
| DELETE | `/api/v1/user` | Sí | Elimina usuario actual |
| POST | `/api/v1/worker` | Sí | Sube archivos y encola inferencia |
| POST | `/api/v1/duplicate` | Sí | Sube docs y encola detección duplicados |
| GET | `/api/v1/task` | Sí | Lista tareas del usuario |
| GET | `/api/v1/task/<task_id>` | Sí | Detalle de tarea |
| DELETE | `/api/v1/task/<task_id>` | Sí | Elimina tarea |
| GET | `/api/v1/export/<task_id>` | Sí | Exporta CSV (normal/no_tecnico) |
| GET/POST/PUT/DELETE | `/api/v1/admin/user` | Sí (admin) | Admin usuarios |
| GET/DELETE | `/api/v1/admin/task` | Sí (admin) | Admin tareas |
| GET/POST/PUT/DELETE | `/api/v1/admin/allowed_ips` | Sí (admin) | Admin IPs permitidas |

## Subida de trabajos

`POST /api/v1/worker`
- Form-data: `files` (múltiples), `task_id` (opcional), `models` (CSV: rode,cude,tilde,legibility)
- Respuesta: `202 { "task_id": "..." }`

`POST /api/v1/duplicate`
- Form-data: `files` (pdf/tif/tiff o zip), `task_id` (opcional)
- Respuesta: `202 { "task_id": "..." }`

## Exportación de resultados

Endpoint: `GET /api/v1/export/<task_id>`

Parámetros de consulta:

- `formato` (opcional): `normal` | `no_tecnico` (por defecto `normal`).

Comportamiento:

- `normal`: exporta todos los documentos de la colección `task_id` tal como se almacenan.
- `no_tecnico`:
  - Incluye solo filas con `nombre_clase` en: `inclinado`, `rotado`, `con corte de informacion`, `no legible`.
  - Requiere `confianza` ≥ 80% (se aceptan valores como "97.33%" o numéricos; si ≤1 se asume fracción y se multiplica por 100).
  - `confianza` se muestra como etiqueta:
    - ≥95% → `muy probable`
    - 90–95% → `probable`
    - 80–90% → `quizás con error`
  - Excluye columnas: `_id`, `modelo`, `tiempo_inferencia`.
  - Orden de columnas: `task_id`, `nombre_archivo`, `pagina`, `nombre_clase`, `confianza`.

Ejemplos:

```bash
curl -H "Authorization: Bearer <TOKEN>" \
  "https://<host>/api/v1/export/ABC123"

curl -H "Authorization: Bearer <TOKEN>" \
  "https://<host>/api/v1/export/ABC123?formato=no_tecnico"
```

## Diagramas

- Vista de endpoints: `api/docs/endpoints-overview.mmd`
- Flujo de exportación: `api/docs/sequence-export.mmd`
- Modelo de datos: `api/docs/er-model.mmd`
- Flujo de autenticación: `api/docs/auth-flow.mmd`

