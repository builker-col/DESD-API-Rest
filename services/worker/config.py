import os
import yaml


def _load_yaml(path: str) -> dict:
    with open(path, 'r', encoding='utf-8') as f:
        return yaml.safe_load(f)


class Config:
    """Configuración del worker unificado multi-modelo."""

    # RabbitMQ configuration
    RABBITMQ_HOST = os.getenv('RABBITMQ_HOST', 'rabbitmq')
    RABBITMQ_USER = os.getenv('RABBITMQ_USER', 'user')
    RABBITMQ_PASS = os.getenv('RABBITMQ_PASS', 'password')
    QUEUE_NAME = os.getenv('QUEUE_NAME', 'queue_worker')

    # PostgreSQL configuration
    POSTGRES_URI = os.getenv('POSTGRES_URI', 'postgresql://user:password@postgres:5432/desd')

    # Carpeta raíz de modelos dentro del contenedor
    MODELS_DIR = os.getenv('MODELS_DIR', '/models')

    # Lista de modelos a cargar en memoria
    ENABLED_MODELS = [
        m.strip()
        for m in os.getenv('ENABLED_MODELS', 'cude,rode,legibility,tilde').split(',')
        if m.strip()
    ]

    _DEFAULT_FILES = {
        'cude': ('cude_v4.onnx', 'cude_v4.xml'),
        'rode': ('rode_v3.onnx', 'rode_v3.xml'),
        'legibility': ('legibility_v2.onnx', 'legibility_v2.xml'),
        'tilde': ('tilde_v2.onnx', 'tilde_v2.xml'),
    }

    MODELS: dict[str, dict] = {}
    for _name in ENABLED_MODELS:
        _onnx_file, _xml_file = _DEFAULT_FILES[_name]
        _base = os.path.join(MODELS_DIR, _name)
        _onnx_path = os.getenv(f'MODEL_{_name.upper()}_ONNX_PATH', os.path.join(_base, _onnx_file))
        _xml_path = os.getenv(f'MODEL_{_name.upper()}_OPENVINO_PATH', os.path.join(_base, _xml_file))
        _meta_path = os.getenv(f'MODEL_{_name.upper()}_METADATA_PATH', os.path.join(_base, 'metadata.yaml'))
        _meta = _load_yaml(_meta_path)
        MODELS[_name] = {
            'onnx_path': _onnx_path,
            'openvino_path': _xml_path,
            'metadata': _meta,
            'imgsz': tuple(_meta['imgsz']),
            'names': _meta['names'],
        }
