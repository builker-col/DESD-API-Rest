"""Servicio gRPC de conversión de documentos a imágenes JPEG.

Convierte archivos PDF y TIFF/TIF en una lista de imágenes JPEG en memoria.
Expuesto mediante gRPC con límites de mensaje de hasta 1GB para soportar
documentos grandes. Diseñado para uso interno por el microservicio API.
"""

from concurrent import futures
import grpc
import converter_pb2
import converter_pb2_grpc
from pdf2image import convert_from_bytes
from PIL import Image, ImageSequence
import time
import io
import os

class ConverterServicer(converter_pb2_grpc.ConverterServicer):
    """Implementación del servicio Converter.

    Métodos:
        ConvertFile: recibe bytes de archivo y devuelve imágenes JPEG.
    """
    def ConvertFile(self, request, context):
        """Convierte un archivo PDF o TIFF en imágenes JPEG.

        Args:
            request (converter_pb2.ConvertRequest):
                - file_content: bytes del archivo de entrada.
                - file_name: nombre del archivo (usado para inferir el tipo: .pdf, .tif, .tiff).
            context: contexto gRPC.

        Returns:
            converter_pb2.ConvertResponse: lista de imágenes JPEG (bytes) en el orden de páginas.

        Notas:
            - Requiere Poppler instalado para PDF.
            - Para TIFF multipágina se itera con PIL.ImageSequence.
            - El tamaño máximo de mensaje está configurado en 1GB (ver función serve()).
        """
        file_content = request.file_content
        file_name = request.file_name.lower()
        images = []

        start_time = time.time()
        if file_name.endswith('.pdf'):
            # PDF → lista de PIL.Image
            images = convert_from_bytes(file_content, thread_count=os.cpu_count(), fmt='jpeg')
        elif file_name.endswith('.tif') or file_name.endswith('.tiff'):
            # TIFF multipágina → iterar páginas y convertir a RGB
            with io.BytesIO(file_content) as file:
                tiff_image = Image.open(file)
                images = [page.convert("RGB") for page in ImageSequence.Iterator(tiff_image)]

        jpg_images = []
        for image in images:
            with io.BytesIO() as output:
                image.save(output, format="JPEG")
                jpg_images.append(output.getvalue())
        
        print(f"Converted {len(jpg_images)} images in {time.time() - start_time} seconds")

        return converter_pb2.ConvertResponse(images=jpg_images)

def serve():
    """Inicializa y arranca el servidor gRPC en 0.0.0.0:50051.

    Configura límites de tamaño de mensaje de 1GB para envíos y recepciones, y
    usa un ThreadPool con max_workers = núm. de CPUs disponibles.
    """
    # 1GB
    max_message_length = 1024 * 1024 * 1024
    server = grpc.server(
        futures.ThreadPoolExecutor(max_workers=os.cpu_count()),
        options=[
            ('grpc.max_send_message_length', max_message_length),
            ('grpc.max_receive_message_length', max_message_length),
        ]
    )
    converter_pb2_grpc.add_ConverterServicer_to_server(ConverterServicer(), server)
    server.add_insecure_port('[::]:50051')
    server.start()
    server.wait_for_termination()

if __name__ == '__main__':
    serve()