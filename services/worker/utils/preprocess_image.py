import numpy as np
import cv2


def load_image(image_path: str) -> np.ndarray:
    """Lee y decodifica una imagen desde disco una sola vez."""
    img = cv2.imread(image_path, cv2.IMREAD_COLOR)
    if img is None:
        raise ValueError(f"Could not decode image: {image_path}")
    return img


def preprocess_array(img: np.ndarray, width: int = 640, height: int = 640) -> np.ndarray:
    """Genera el tensor NCHW float32 [0, 1] usando cv2.dnn.blobFromImage (C++/SIMD)."""
    return cv2.dnn.blobFromImage(
        img,
        scalefactor=1.0 / 255.0,
        size=(width, height),
        swapRB=False,
        crop=False,
    )


def preprocess_image(image_path: str, width: int = 640, height: int = 640) -> np.ndarray:
    """Preprocesa una imagen directamente desde su ruta en disco."""
    return preprocess_array(load_image(image_path), width, height)
