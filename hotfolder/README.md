# Hot-Folder Local — Guía de Operación

El servicio **Hot-Folder** (integrado en `services/api`, en Go) permite procesar lotes masivos de documentos directamente desde disco local **sin subida HTTP**, con conversión paralela a `/shared-data` (`tmpfs`), seguimiento en PostgreSQL y exportación automática de reportes CSV.

---

## Estructura de carpetas

```text
hotfolder/
├── inbox/        # Entrada: deposita aquí los archivos (o una subcarpeta por lote) + trigger
├── processing/   # Lotes actualmente en ejecución (movidos atómicamente para evitar reproceso)
├── done/         # Archivos originales completados (cuando action=move)
├── failed/       # Archivos corruptos o triggers inválidos (incluye error.txt)
└── results/      # Reportes CSV generados atómicamente al completar cada lote
```

---

## Flujo de uso

### Modo recomendado: subcarpeta por lote (soporta múltiples lotes simultáneos)
1. Crea una subcarpeta dentro de `hotfolder/inbox/`, por ejemplo `hotfolder/inbox/lote_facturas_01/`.
2. Copia todos tus documentos (`pdf`, `tif`, `tiff`, `jpg`, `jpeg`, `png`, `zip`) dentro de esa subcarpeta.
3. **Al finalizar la copia**, crea o copia el archivo **`trigger.txt`** (o **`trigger.json`**) dentro de la misma subcarpeta.

### Modo rápido: carpeta plana
También puedes copiar archivos y `trigger.txt` directamente en la raíz de `hotfolder/inbox/`.

> **Importante:** coloca siempre el archivo trigger **al final**, una vez que todos los documentos hayan terminado de copiarse. El watcher ignora archivos temporales (`*.tmp`, `*.part`, `*.crdownload`, `._*`, `.DS_Store`).

---

## Formato del archivo trigger

### Opción 1: `trigger.txt` (`clave=valor`)
```ini
# Modelos de inferencia: cude,rode,legibility,tilde (o "dude" para detección de duplicados)
models=cude,rode,legibility,tilde

# Acción sobre los originales al terminar: move | delete | keep
action=move

# Destino cuando action=move (ruta relativa a /hotfolder o absoluta; defecto: done)
destination=done

# Exportación automática de CSV en results/: none | normal | no_tecnico | both
export=both

# Opcionales: si se omiten, usa el nombre de la subcarpeta (o autogenera) y el usuario admin
# task_id=lote_noviembre_01
# user=admin
```
*(También acepta una sola línea simple con la lista de modelos, por ejemplo: `cude,rode,legibility,tilde`.)*

### Opción 2: `trigger.json`
```json
{
  "models": ["cude", "rode", "legibility", "tilde"],
  "action": "delete",
  "export": "both",
  "task_id": "lote_noviembre_01",
  "user": "admin"
}
```

### Ejemplo para detección de duplicados (`dude`)
```ini
models=dude
action=move
export=normal
```

---

## Resultados y trazabilidad

- **Archivos CSV:** se escriben atómicamente (`.tmp` → `.csv`) en `hotfolder/results/`:
  - `<task_id>.csv` (formato `normal`)
  - `<task_id>_no_tecnico.csv` (formato `no_tecnico`, filtrado a hallazgos problemáticos con confianza `>= 80%`)
- **Visibilidad en API REST:** todos los lotes procesados por hot-folder quedan registrados en PostgreSQL bajo el `user` configurado, por lo que pueden consultarse y descargarse también desde `GET /api/v1/task` y `GET /api/v1/export/{task_id}`.
- **Aislamiento de fallos:** si un archivo dentro de un lote de 100 documentos está corrupto, se mueve únicamente ese archivo a `hotfolder/failed/<task_id>/` con el detalle en `error.txt`, y los otros 99 se procesan con normalidad.

Plantilla lista para copiar: [trigger.example.txt](trigger.example.txt).
