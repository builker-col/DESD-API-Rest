from datetime import datetime
import time
from typing import Any, Dict, List

from sqlalchemy import MetaData, create_engine, insert, select, update
from sqlalchemy.engine import Engine
from sqlalchemy.orm import sessionmaker

from config import Config


class DatabaseService:
    """Gestiona el acceso exclusivo a PostgreSQL para resultados y estados de duplicados."""

    def __init__(self) -> None:
        self.engine: Engine | None = None
        self.metadata: MetaData | None = None
        self.results_table = None
        self.result_status_table = None
        self.result_item_table = None
        self.SessionLocal = None

        # Conectar a PostgreSQL con pool_pre_ping para tolerar reconexiones
        self.engine = create_engine(Config.POSTGRES_URI, future=True, pool_pre_ping=True)
        self.metadata = MetaData()

        # Esperar a que existan las tablas creadas por el API
        required_tables = {"result", "result_status", "result_item"}
        max_attempts = 30
        for attempt in range(1, max_attempts + 1):
            assert self.metadata is not None and self.engine is not None
            self.metadata.clear()
            self.metadata.reflect(bind=self.engine)
            existing = set(self.metadata.tables.keys())
            missing = required_tables - existing
            if not missing:
                break
            wait_time = 2
            print(
                f"[DatabaseService] Tablas faltantes en Postgres {missing} (intento {attempt}/{max_attempts}). Reintentando en {wait_time}s..."
            )
            time.sleep(wait_time)

        if not required_tables.issubset(set(self.metadata.tables.keys())):
            raise RuntimeError(
                "No se encontraron las tablas requeridas 'result', 'result_status' y 'result_item' en Postgres"
            )

        self.results_table = self.metadata.tables["result"]
        self.result_status_table = self.metadata.tables["result_status"]
        self.result_item_table = self.metadata.tables["result_item"]

        self.SessionLocal = sessionmaker(bind=self.engine, future=True)

    def save_result_item(self, task_id: Any, data: Dict[str, Any]) -> None:
        """Inserta un registro de resultado en la tabla result_item."""
        self.save_result_items(task_id, [data])

    def save_result_items(self, task_id: Any, items: List[Dict[str, Any]]) -> None:
        """Inserta uno o varios registros de resultados en la tabla result_item en una sola transacción."""
        if not items:
            return
        if self.SessionLocal is None:
            raise RuntimeError("La fábrica de sesiones de base de datos no está inicializada")

        task_id_str = str(task_id)
        now = datetime.now()
        with self.SessionLocal() as session:
            stmt = select(self.results_table.c.id).where(
                self.results_table.c.collection_id == task_id_str
            )
            row = session.execute(stmt).first()
            result_pk = row[0] if row else None

            rows = [
                {
                    "result_id": result_pk,
                    "task_id": task_id_str,
                    "data": item,
                    "created_at": now,
                }
                for item in items
            ]
            session.execute(insert(self.result_item_table), rows)
            session.commit()

    def update_result_status(self, result_id: Any, **kwargs: Any) -> None:
        """Actualiza campos en la tabla result_status para un result dado por collection_id."""
        if self.SessionLocal is None:
            raise RuntimeError("La fábrica de sesiones de base de datos no está inicializada")

        with self.SessionLocal() as session:
            stmt = select(self.results_table.c.id).where(
                self.results_table.c.collection_id == str(result_id)
            )
            row = session.execute(stmt).first()
            if not row:
                print(
                    f"[DatabaseService] No se encontró result con collection_id={result_id} para actualizar status."
                )
                return
            result_pk = row[0]

            upd = (
                update(self.result_status_table)
                .where(self.result_status_table.c.result_id == result_pk)
                .values(**kwargs)
            )
            session.execute(upd)
            session.commit()

    def get_result_status(self, result_id: Any) -> Dict[str, Any]:
        """Obtiene el estado asociado a un result vía collection_id."""
        if self.SessionLocal is None:
            raise RuntimeError("La fábrica de sesiones de base de datos no está inicializada")

        with self.SessionLocal() as session:
            stmt = select(self.results_table.c.id).where(
                self.results_table.c.collection_id == str(result_id)
            )
            row = session.execute(stmt).first()
            if not row:
                print(
                    f"[DatabaseService] No se encontró result con collection_id={result_id} al consultar status."
                )
                return {"total_files": 0, "total_files_processed": 0, "status": "PENDING"}
            result_pk = row[0]

            st = select(
                self.result_status_table.c.total_files,
                self.result_status_table.c.total_files_processed,
                self.result_status_table.c.status,
            ).where(self.result_status_table.c.result_id == result_pk)
            rs = session.execute(st).first()
            if not rs:
                return {"total_files": 0, "total_files_processed": 0, "status": "PENDING"}

            total_files, total_files_processed, status_val = rs
            status_str = getattr(status_val, "value", status_val)
            return {
                "total_files": total_files or 0,
                "total_files_processed": total_files_processed or 0,
                "status": status_str or "PENDING",
            }

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
