"""Worker unificado multi-modelo con pipeline solapado de 3 etapas:

Etapa 1 (Prefetch threads): lee y decodifica la página N+1 y genera tensores compartidos (960/640) en paralelo.
Etapa 2 (Inference thread): ejecuta todos los modelos solicitados en paralelo sobre tensores ya listos.
Etapa 3 (Batched DB writer): agrupa páginas terminadas en 1 bulk INSERT + 1 UPDATE atómico en PostgreSQL,
                             elimina las imágenes de /shared-data (tmpfs) y envía ACK thread-safe a RabbitMQ.
"""

import json
import os
import queue
import threading
import time
from config import Config
from services import *
from utils.preprocess_image import load_image

PREFETCH_THREADS = int(os.getenv('WORKER_PREFETCH_THREADS', '2'))
DB_BATCH_SIZE = int(os.getenv('WORKER_DB_BATCH_SIZE', '8'))
DB_FLUSH_WINDOW_MS = float(os.getenv('WORKER_DB_FLUSH_WINDOW_MS', '15'))

messaging_service = MessagingService()
inference_service = InferenceService()
database_service = DatabaseService()

CLASS_TRANSLATION = {
    'tilted': 'inclinado',
    'no tilted': 'no inclinado',
    'rotated': 'rotado',
    'no_rotated': 'no rotado',
    'cut': 'con corte de informacion',
    'no_cut': 'sin corte de informacion',
    'unreadable': 'no legible',
    'readable': 'legible',
}

raw_queue: queue.Queue = queue.Queue(maxsize=16)
ready_queue: queue.Queue = queue.Queue(maxsize=6)
db_queue: queue.Queue = queue.Queue(maxsize=32)


def postprocess(model_name: str, results):
    """Postprocesa la salida de un modelo específico."""
    output = results[0][0]
    raw_class = Config.MODELS[model_name]['names'][int(output.argmax())]
    class_name = CLASS_TRANSLATION.get(raw_class, raw_class)
    return output, class_name


def _cleanup_file(file_path: str) -> None:
    """Elimina la imagen procesada y remueve el directorio de la tarea si ya quedó vacío."""
    try:
        os.remove(file_path)
    except FileNotFoundError:
        pass
    except OSError as exc:
        print(f"[cleanup] No se pudo eliminar {file_path}: {exc}", flush=True)

    parent_dir = os.path.dirname(file_path)
    if parent_dir and parent_dir.startswith('/shared-data/') and parent_dir != '/shared-data':
        try:
            os.rmdir(parent_dir)
        except OSError:
            pass


def _prefetch_loop() -> None:
    """Etapa 1: decodifica JPEG y genera blobs NCHW float32 mientras la etapa 2 infiere la página previa."""
    while True:
        delivery_tag, task = raw_queue.get()
        file_path = task['file_path']
        models = task.get('models') or ([task['model_name']] if 'model_name' in task else [])
        try:
            if not os.path.exists(file_path):
                raise FileNotFoundError(f"File not found: {file_path}")
            img = load_image(file_path)
            valid_models, blobs_by_size = inference_service.prepare_blobs(img, models)
            ready_queue.put((delivery_tag, task, valid_models, blobs_by_size))
        except Exception as exc:
            print(f"[prefetch] Error en {file_path}: {exc}", flush=True)
            _cleanup_file(file_path)
            try:
                database_service.record_failed_page(
                    task_id=task.get('task_id', ''),
                    failed_items=max(1, len(models)),
                    current_file=task.get('filename', ''),
                    file_index=task.get('file_index') or 0,
                )
            except Exception as db_exc:
                print(f"[prefetch] Error actualizando contador tras fallo: {db_exc}", flush=True)
            messaging_service.nack_threadsafe(delivery_tag, requeue=False)
        finally:
            raw_queue.task_done()


def _inference_loop() -> None:
    """Etapa 2: infiere todos los modelos solicitados en paralelo sobre tensores ya preprocesados."""
    while True:
        delivery_tag, task, valid_models, blobs_by_size = ready_queue.get()
        try:
            inferences = inference_service.infer_prepared(valid_models, blobs_by_size)
            items = []
            for model_name, raw_results, inference_time in inferences:
                output_data, class_name = postprocess(model_name, raw_results)
                items.append({
                    'task_id': task['task_id'],
                    'nombre_archivo': task['filename'],
                    'pagina': task['page'],
                    'confianza': f"{float(output_data.max()) * 100:.2f}%",
                    'nombre_clase': class_name,
                    'modelo': model_name,
                    'tiempo_inferencia': inference_time,
                })
            db_queue.put((delivery_tag, task, items))
        except Exception as exc:
            print(f"[inference] Error en {task.get('file_path')}: {exc}", flush=True)
            _cleanup_file(task['file_path'])
            try:
                models_list = task.get('models') or ([task['model_name']] if 'model_name' in task else [])
                database_service.record_failed_page(
                    task_id=task.get('task_id', ''),
                    failed_items=max(1, len(models_list)),
                    current_file=task.get('filename', ''),
                    file_index=task.get('file_index') or 0,
                )
            except Exception as db_exc:
                print(f"[inference] Error actualizando contador tras fallo: {db_exc}", flush=True)
            messaging_service.nack_threadsafe(delivery_tag, requeue=False)
        finally:
            ready_queue.task_done()


def _db_writer_loop() -> None:
    """Etapa 3: agrupa páginas en micro-lotes (1 transacción PG), borra temporales y envía ACK."""
    flush_window_s = DB_FLUSH_WINDOW_MS / 1000.0
    while True:
        first = db_queue.get()
        batch = [first]
        deadline = time.perf_counter() + flush_window_s

        while len(batch) < DB_BATCH_SIZE:
            remaining = deadline - time.perf_counter()
            if remaining <= 0:
                break
            try:
                batch.append(db_queue.get(timeout=remaining))
            except queue.Empty:
                break

        records = [
            {
                'task_id': task['task_id'],
                'items': items,
                'current_file': task['filename'],
                'file_index': task['file_index'],
            }
            for (_tag, task, items) in batch
        ]

        try:
            database_service.flush_page_batch(records)
            for delivery_tag, task, _items in batch:
                _cleanup_file(task['file_path'])
                messaging_service.ack_threadsafe(delivery_tag)
            print(
                f"[db_writer] Flushed batch of {len(batch)} page(s) ({sum(len(r['items']) for r in records)} rows)",
                flush=True,
            )
        except Exception as exc:
            print(f"[db_writer] Error guardando lote en Postgres: {exc}", flush=True)
            for delivery_tag, _task, _items in batch:
                messaging_service.nack_threadsafe(delivery_tag, requeue=True)
        finally:
            for _ in batch:
                db_queue.task_done()


def callback(ch, method, properties, body):
    """Recibe el mensaje de RabbitMQ y lo inyecta sin bloquear al pipeline de prefetch."""
    task = json.loads(body)
    raw_queue.put((method.delivery_tag, task))


# Lanzar hilos del pipeline de 3 etapas
for _i in range(max(1, PREFETCH_THREADS)):
    threading.Thread(target=_prefetch_loop, daemon=True, name=f"prefetch-{_i}").start()
threading.Thread(target=_inference_loop, daemon=True, name="inference").start()
threading.Thread(target=_db_writer_loop, daemon=True, name="db-writer").start()

# Conectar a RabbitMQ y consumir tareas en el hilo principal
messaging_service.consume(callback)
