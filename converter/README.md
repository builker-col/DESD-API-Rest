# Converter (gRPC)

Servicio gRPC para convertir documentos PDF/TIFF en imágenes JPEG. Utiliza `pdf2image` y `Pillow` para la conversión y expone un método RPC `ConvertFile`.

## Capacidades
- Entrada: PDF, TIF, TIFF
- Salida: lista de imágenes JPEG (bytes)
- Procesamiento multi-hilo (usa `os.cpu_count()`)
- Límite de mensaje gRPC configurado a 1GB (envío y recepción)

## Requisitos
- Python 3.10+
- Dependencias de Python: ver `requirements.txt`
- Poppler (solo para PDF)
  - Linux: `sudo apt-get install -y poppler-utils`
  - macOS: `brew install poppler`
  - Windows: descarga Poppler y agrega la carpeta `bin` al `PATH` (p.ej. `C:\poppler\bin`)

## Instalación
```bash
# Crear entorno
python -m venv .venv
source .venv/bin/activate  # Windows: .venv\Scripts\activate

# Instalar dependencias
pip install -r converter/requirements.txt
```

## Ejecución local
```bash
python -m converter.app
# El servidor escuchará en 0.0.0.0:50051 (inseguro)
```

## API gRPC
Archivo `converter/converter.proto`:
```proto
syntax = "proto3";
package converter;
service Converter {
  rpc ConvertFile(ConvertRequest) returns (ConvertResponse);
}
message ConvertRequest {
  bytes file_content = 1; // contenido del archivo
  string file_name = 2;   // nombre/extension (usa para decidir PDF o TIFF)
}
message ConvertResponse {
  repeated bytes images = 1; // lista de imágenes JPEG
}
```

Servidor (`converter/app.py`):
- Usa `pdf2image.convert_from_bytes` para PDF y `PIL.ImageSequence` para TIFF multi-página.
- Configura opciones gRPC:
  - `grpc.max_send_message_length`: 1GB
  - `grpc.max_receive_message_length`: 1GB

## Cliente de ejemplo (Python)
```python
import grpc
import converter_pb2, converter_pb2_grpc

opts = [
    ('grpc.max_send_message_length', 1024 * 1024 * 1024),
    ('grpc.max_receive_message_length', 1024 * 1024 * 1024),
]

with grpc.insecure_channel('localhost:50051', options=opts) as channel:
    stub = converter_pb2_grpc.ConverterStub(channel)
    with open('documento.pdf', 'rb') as f:
        req = converter_pb2.ConvertRequest(
            file_content=f.read(),
            file_name='documento.pdf'
        )
    res = stub.ConvertFile(req)
    print(f"Imágenes recibidas: {len(res.images)}")
    # Guardar primera imagen
    if res.images:
        with open('pagina_1.jpg', 'wb') as out:
            out.write(res.images[0])
```

## Regenerar stubs gRPC
Si modificas `converter.proto`, regenera los stubs de Python:
```bash
python -m grpc_tools.protoc \
  -I converter \
  --python_out=converter \
  --grpc_python_out=converter \
  converter/converter.proto
```
Asegúrate de que `grpcio-tools` esté instalado (ver `requirements.txt`).

## Despliegue con Docker
Este servicio se integra en `docker-compose.yml` como `converter` y es consumido por `api`.

- Servicio `converter` publicado en `50051`.
- La API se conecta usando `CONVERTER_GRPC_HOST=converter:50051`.

Ejemplo mínimo de servicio en compose:
```yaml
converter:
  build:
    context: .
    dockerfile: Dockerfile.converter
  ports:
    - "50051:50051"
```

## Errores comunes y troubleshooting
- "Poppler not installed": instala Poppler y verifica que `pdftoppm` esté en el `PATH`.
- "MAX_MESSAGE_LENGTH exceeded": el archivo es demasiado grande; considera dividirlo.
- "Unsupported file": asegúrate de usar extensiones `.pdf`, `.tif`, `.tiff`.
- Tiempo de conversión alto: valida recursos disponibles (CPU), reduce resolución por defecto si fuera necesario (parámetros de `pdf2image`).

## Rendimiento y consideraciones
- El servidor usa `max_workers=os.cpu_count()`; ajusta según tu entorno.
- Los límites de 1GB permiten archivos grandes pero aumentan uso de memoria.
- Para producción, considera TLS en gRPC y políticas de tamaño más restrictivas.
