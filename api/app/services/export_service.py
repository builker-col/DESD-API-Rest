import csv
import os
import tempfile
from app import mongo

class ExportService:
    """Servicio de exportación de resultados a CSV.

    export_collection(collection_name, formato)
    Args:
        collection_name: Nombre de la colección Mongo (task_id).
        formato: 'normal' o 'no_tecnico'.
    Returns:
        Ruta temporal al archivo CSV generado.
    """
    def export_collection(self, collection_name: str, formato: str = 'normal') -> str:
        collection = mongo[collection_name]
        if collection.count_documents({}) == 0:
            return None

        tmpdir = tempfile.mkdtemp()
        filename = f"{collection_name}.csv"
        filepath = os.path.join(tmpdir, filename)

        if formato == 'no_tecnico':
            return self._export_non_technical(collection, filepath)
        else:
            return self._export_normal(collection, filepath)

    def _export_normal(self, collection, filepath: str) -> str:
        """Exporta todos los documentos tal como están guardados en Mongo.

        Si la colección no tiene documentos, devuelve None.
        """
        first_doc = collection.find_one()
        if not first_doc:
            return None

        with open(filepath, "w", newline='') as f:
            writer = csv.DictWriter(f, fieldnames=list(first_doc.keys()))
            writer.writeheader()
            for document in collection.find():
                writer.writerow(document)
        return filepath

    def _export_non_technical(self, collection, filepath: str) -> str:
        """Exporta CSV filtrado y simplificado para usuarios no técnicos.

        - Filtra por clases problemáticas.
        - Mantiene registros con confianza >= 80%.
        - Reemplaza confianza por etiqueta legible.
        - Excluye columnas sensibles o técnicas.
        - Usa un orden de columnas estable.
        """
        def parse_conf(conf):
            if isinstance(conf, str):
                value = conf.strip()
                if value.endswith('%'):
                    try:
                        return float(value.replace('%', '').strip())
                    except Exception:
                        return 0.0
                try:
                    v = float(value)
                    return v * 100.0 if v <= 1.0 else v
                except Exception:
                    return 0.0
            try:
                v = float(conf)
                return v * 100.0 if v <= 1.0 else v
            except Exception:
                return 0.0

        def confidence_to_label(p: float) -> str:
            if p >= 95.0:
                return 'muy probable'
            if p >= 90.0:
                return 'probable'
            return 'quizás con error'

        problematic_classes = {
            'inclinado',
            'rotado',
            'con corte de informacion',
            'no legible',
        }

        filtered_rows = []
        for document in collection.find():
            nombre_clase = document.get('nombre_clase')
            if nombre_clase not in problematic_classes:
                continue

            conf_value = parse_conf(document.get('confianza'))
            if conf_value < 80.0:
                continue

            # Excluir columnas no deseadas
            row = {k: v for k, v in document.items() if k not in {'_id', 'modelo', 'tiempo_inferencia'}}
            # Reemplazar confianza por etiqueta
            row['confianza'] = confidence_to_label(conf_value)
            filtered_rows.append(row)

        # Orden de columnas preferido
        preferred_order = ['task_id', 'nombre_archivo', 'pagina', 'nombre_clase', 'confianza']

        with open(filepath, "w", newline='') as f:
            # Cabecera siempre en el orden preferido
            writer = csv.DictWriter(f, fieldnames=preferred_order)
            writer.writeheader()
            for row in filtered_rows:
                # Limitar a columnas de la cabecera; valores faltantes quedarán en blanco
                limited_row = {key: row.get(key) for key in preferred_order}
                writer.writerow(limited_row)

        return filepath