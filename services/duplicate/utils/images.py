import os
from skimage.metrics import structural_similarity as ssim
from PIL import Image
import numpy as np
import imagehash
import cv2

DUPLICATE_SSIM_MAX_DIM = int(os.getenv('DUPLICATE_SSIM_MAX_DIM', '512'))


def get_image_hash(image_path: str) -> imagehash.ImageHash:
    """Calcula el hash perceptual (pHash) de una imagen."""
    with Image.open(image_path) as img:
        return imagehash.phash(img.convert('L').resize((32, 32)))


def load_image_grey(image_path: str, max_dim: int = DUPLICATE_SSIM_MAX_DIM) -> np.ndarray:
    """Carga una imagen en escala de grises y la redimensiona a `max_dim` para acelerar SSIM."""
    img = cv2.imread(image_path, cv2.IMREAD_GRAYSCALE)
    if img is None:
        return None
    if max_dim and max_dim > 0:
        h, w = img.shape[:2]
        largest = max(h, w)
        if largest > max_dim:
            scale = max_dim / float(largest)
            img = cv2.resize(
                img,
                (max(1, int(w * scale)), max(1, int(h * scale))),
                interpolation=cv2.INTER_AREA,
            )
    return img


def compare_image_arrays(img1: np.ndarray, img2: np.ndarray) -> float:
    """Compara dos imágenes en escala de grises ya cargadas y escaladas en memoria usando SSIM."""
    if img1 is None or img2 is None:
        return 0.0
    if img1.shape != img2.shape:
        img2 = cv2.resize(img2, (img1.shape[1], img1.shape[0]), interpolation=cv2.INTER_AREA)
    score, _ = ssim(img1, img2, full=True)
    return float(score)


def compare_images(image1_path: str, image2_path: str) -> float:
    """Compara dos imágenes desde disco y devuelve su similitud SSIM [0, 1]."""
    img1 = load_image_grey(image1_path)
    img2 = load_image_grey(image2_path)
    if img1 is None or img2 is None:
        print(f"Error loading images {image1_path} or {image2_path}")
        return 0.0
    return compare_image_arrays(img1, img2)
