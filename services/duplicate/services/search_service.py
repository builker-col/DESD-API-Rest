from utils import get_image_hash, load_image_grey
from utils.images import compare_image_arrays
from concurrent.futures import ThreadPoolExecutor
from typing import Optional, Dict, List, Tuple


def find_duplicate_pages(
    image_paths: list,
    similarity_threshold: float = 0.95,
    hash_threshold: int = 5,
) -> Tuple[list, List[Dict[str, Optional[str]]]]:
    duplicates = []
    report = []
    num_pages = len(image_paths)
    if num_pages < 2:
        return duplicates, report

    # Calcular hashes perceptuales en paralelo
    with ThreadPoolExecutor() as executor:
        hashes = list(executor.map(get_image_hash, image_paths))

    # Identificar pares candidatos antes de cargar imágenes completas
    candidate_pairs = []
    needed_indices = set()
    for i in range(num_pages):
        for j in range(i + 1, num_pages):
            if hashes[i] - hashes[j] <= hash_threshold:
                candidate_pairs.append((i, j))
                needed_indices.add(i)
                needed_indices.add(j)

    if not candidate_pairs:
        return duplicates, report

    # Cargar en memoria cada imagen candidata UNA sola vez (en paralelo)
    sorted_indices = sorted(needed_indices)
    with ThreadPoolExecutor() as executor:
        loaded = list(executor.map(lambda idx: load_image_grey(image_paths[idx]), sorted_indices))
        grey_cache = dict(zip(sorted_indices, loaded))

        futures = [
            executor.submit(compare_image_arrays, grey_cache[i], grey_cache[j])
            for i, j in candidate_pairs
        ]
        results = [f.result() for f in futures]

    for (i, j), similarity in zip(candidate_pairs, results):
        if similarity > similarity_threshold:
            duplicates.append((i + 1, j + 1, similarity))
            report.append({
                'pagina1': i + 1,
                'pagina2': j + 1,
                'similitud': similarity,
            })

    return duplicates, report
