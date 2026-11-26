from concurrent.futures import ThreadPoolExecutor
import os
import time
from typing import Dict, List, Tuple

import cpuinfo
import numpy as np
import onnxruntime as ort
from openvino import Core

from config import Config
from utils.preprocess_image import preprocess_array


def _default_threads_per_model(num_models: int) -> int:
    env_val = os.getenv('INFERENCE_NUM_THREADS')
    if env_val:
        try:
            return max(1, int(env_val))
        except ValueError:
            pass
    cpus = os.cpu_count() or 4
    return max(1, cpus // max(1, num_models))


class _SingleModelRunner:
    """Encapsula una sesión de inferencia (OpenVINO en Intel o ONNX Runtime en ARM/otros)."""

    def __init__(self, name: str, spec: dict, is_intel_cpu: bool, ie: Core | None, num_threads: int) -> None:
        self.name = name
        self.imgsz: Tuple[int, int] = spec['imgsz']
        self.names: dict = spec['names']
        self.is_intel_cpu = is_intel_cpu

        if is_intel_cpu and ie is not None:
            model = ie.read_model(model=spec['openvino_path'])
            self.compiled_model = ie.compile_model(
                model=model,
                device_name='CPU',
                config={
                    'INFERENCE_NUM_THREADS': str(num_threads),
                    'PERFORMANCE_HINT': 'LATENCY',
                },
            )
            self.session = None
            self.input_name = None
        else:
            sess_options = ort.SessionOptions()
            sess_options.graph_optimization_level = ort.GraphOptimizationLevel.ORT_ENABLE_ALL
            sess_options.execution_mode = ort.ExecutionMode.ORT_SEQUENTIAL
            sess_options.intra_op_num_threads = num_threads
            sess_options.inter_op_num_threads = 1
            self.session = ort.InferenceSession(
                spec['onnx_path'],
                sess_options=sess_options,
                providers=['CPUExecutionProvider'],
            )
            self.input_name = self.session.get_inputs()[0].name
            self.compiled_model = None

    def infer(self, blob: np.ndarray) -> Tuple[any, float]:
        start_time = time.perf_counter()
        if self.is_intel_cpu:
            results = self.compiled_model(blob)
        else:
            results = self.session.run(None, {self.input_name: blob})
        return results, time.perf_counter() - start_time


class InferenceService:
    """Servicio multi-modelo con separación entre preprocesamiento (prefetch) e inferencia paralela."""

    def __init__(self) -> None:
        cpu_info = cpuinfo.get_cpu_info()
        self.is_intel_cpu = 'Intel' in cpu_info.get('brand_raw', '')
        ie = Core() if self.is_intel_cpu else None
        num_threads = _default_threads_per_model(len(Config.MODELS))

        self.runners: Dict[str, _SingleModelRunner] = {}
        for name, spec in Config.MODELS.items():
            self.runners[name] = _SingleModelRunner(
                name=name,
                spec=spec,
                is_intel_cpu=self.is_intel_cpu,
                ie=ie,
                num_threads=num_threads,
            )
            print(f"[InferenceService] Loaded model '{name}' (imgsz={spec['imgsz']}, threads={num_threads})", flush=True)

        self.executor = ThreadPoolExecutor(max_workers=max(1, len(self.runners)))

    def prepare_blobs(
        self,
        img: np.ndarray,
        model_names: List[str],
    ) -> Tuple[List[str], Dict[Tuple[int, int], np.ndarray]]:
        """Preprocesa cada resolución única UNA sola vez (ideal para hilo de prefetch)."""
        valid_names = [m for m in model_names if m in self.runners]
        blobs_by_size: Dict[Tuple[int, int], np.ndarray] = {}
        for name in valid_names:
            size = self.runners[name].imgsz
            if size not in blobs_by_size:
                blobs_by_size[size] = preprocess_array(img, *size)
        return valid_names, blobs_by_size

    def infer_prepared(
        self,
        valid_names: List[str],
        blobs_by_size: Dict[Tuple[int, int], np.ndarray],
    ) -> List[Tuple[str, any, float]]:
        """Ejecuta los modelos solicitados en paralelo sobre los tensores ya preparados."""
        if not valid_names:
            return []

        if len(valid_names) == 1:
            name = valid_names[0]
            runner = self.runners[name]
            res, elapsed = runner.infer(blobs_by_size[runner.imgsz])
            return [(name, res, elapsed)]

        futures = [
            (name, self.executor.submit(self.runners[name].infer, blobs_by_size[self.runners[name].imgsz]))
            for name in valid_names
        ]
        output = []
        for name, fut in futures:
            res, elapsed = fut.result()
            output.append((name, res, elapsed))
        return output

    def infer_multi(
        self,
        img: np.ndarray,
        model_names: List[str],
    ) -> List[Tuple[str, any, float]]:
        valid_names, blobs_by_size = self.prepare_blobs(img, model_names)
        return self.infer_prepared(valid_names, blobs_by_size)
