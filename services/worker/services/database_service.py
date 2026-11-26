from collections import OrderedDict
from datetime import datetime
import time
from typing import Any, Dict, List

from sqlalchemy import MetaData, create_engine, insert, select, text
from sqlalchemy.engine import Engine
from sqlalchemy.orm import sessionmaker

from config import Config


class DatabaseService:
    """Gestiona escritura en lote y contadores atómicos en PostgreSQL sin SELECT COUNT(*)."""

    def __init__(self) -> None:
        self.engine: Engine | None = None
        self.metadata: MetaData | None = None
        self.results_table = None
        self.result_status_table = None
        self.result_item_table = None
        self.SessionLocal = None
        self._result_pk_cache: OrderedDict[str, int] = OrderedDict()

        self.engine = create_engine(Config.POSTGRES_URI, future=True, pool_pre_ping=True)
        self.metadata = MetaData()

        required_tables = {"result", "result_status", "result_item"}
        required_status_cols = {"total_items_expected", "total_items_processed"}
        max_attempts = 30
        for attempt in range(1, max_attempts + 1):
            assert self.metadata is not None and self.engine is not None
            self.metadata.clear()
            self.metadata.reflect(bind=self.engine)
            existing = set(self.metadata.tables.keys())
            missing = required_tables - existing
            if not missing:
                rs_cols = set(self.metadata.tables["result_status"].c.keys())
                if required_status_cols.issubset(rs_cols):
                    break
                print(
                    f"[DatabaseService] Esperando columnas atómicas en result_status (intento {attempt}/{max_attempts})...",
                    flush=True,
                )
            else:
                print(
                    f"[DatabaseService] Tablas faltantes en Postgres {missing} (intento {attempt}/{max_attempts})...",
                    flush=True,
                )
            time.sleep(2)

        if not required_tables.issubset(set(self.metadata.tables.keys())):
            raise RuntimeError(
                "No se encontraron las tablas requeridas 'result', 'result_status' y 'result_item' en Postgres"
            )

        self.results_table = self.metadata.tables["result"]
        self.result_status_table = self.metadata.tables["result_status"]
        self.result_item_table = self.metadata.tables["result_item"]

        self.SessionLocal = sessionmaker(bind=self.engine, future=True)

    def _get_result_pk(self, session, task_id_str: str) -> int | None:
        cached = self._result_pk_cache.get(task_id_str)
        if cached is not None:
            self._result_pk_cache.move_to_end(task_id_str)
            return cached

        stmt = select(self.results_table.c.id).where(
            self.results_table.c.collection_id == task_id_str
        )
        row = session.execute(stmt).first()
        if not row:
            return None

        pk = row[0]
        self._result_pk_cache[task_id_str] = pk
        if len(self._result_pk_cache) > 1000:
            self._result_pk_cache.popitem(last=False)
        return pk

    def flush_page_batch(self, records: List[Dict[str, Any]]) -> None:
        """Persiste múltiples páginas en UNA sola transacción: 1 bulk INSERT + 1 UPDATE atómico por tarea."""
        if not records:
            return
        if self.SessionLocal is None:
            raise RuntimeError("La fábrica de sesiones de base de datos no está inicializada")

        now = datetime.now()
        with self.SessionLocal() as session:
            all_rows = []
            task_agg: Dict[str, Dict[str, Any]] = {}

            for rec in records:
                task_id_str = str(rec['task_id'])
                items = rec['items']
                if not items:
                    continue

                result_pk = self._get_result_pk(session, task_id_str)
                for item in items:
                    all_rows.append({
                        'result_id': result_pk,
                        'task_id': task_id_str,
                        'data': item,
                        'created_at': now,
                    })

                if result_pk is not None:
                    agg = task_agg.get(task_id_str)
                    if agg is None:
                        task_agg[task_id_str] = {
                            'result_pk': result_pk,
                            'added': len(items),
                            'max_file_index': rec.get('file_index') or 0,
                            'current_file': rec.get('current_file') or '',
                        }
                    else:
                        agg['added'] += len(items)
                        fidx = rec.get('file_index') or 0
                        if fidx >= agg['max_file_index']:
                            agg['max_file_index'] = fidx
                            agg['current_file'] = rec.get('current_file') or agg['current_file']

            if all_rows:
                session.execute(insert(self.result_item_table), all_rows)

            atomic_upd = text(
                """
                UPDATE result_status
                SET total_items_processed = COALESCE(total_items_processed, 0) + :added,
                    total_files_processed = GREATEST(COALESCE(total_files_processed, 0), :max_file_idx),
                    current_file = :current_file,
                    status = CASE
                        WHEN total_items_expected IS NOT NULL
                             AND (COALESCE(total_items_processed, 0) + :added) >= total_items_expected
                            THEN 'COMPLETED'::resultstatusenum
                        ELSE 'RUNNING'::resultstatusenum
                    END,
                    last_updated = :now
                WHERE result_id = :result_pk
                """
            )
            for agg in task_agg.values():
                session.execute(
                    atomic_upd,
                    {
                        'added': agg['added'],
                        'max_file_idx': agg['max_file_index'],
                        'current_file': agg['current_file'],
                        'now': now,
                        'result_pk': agg['result_pk'],
                    },
                )

            session.commit()

    def close(self) -> None:
        try:
            if self.engine is not None:
                self.engine.dispose()
        except Exception as exc:
            print(f"[DatabaseService] Error cerrando motor de Postgres: {exc}")

    def __del__(self) -> None:
        try:
            self.close()
        except Exception:
            pass
