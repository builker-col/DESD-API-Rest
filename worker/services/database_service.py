import time
from typing import Any, Dict

from pymongo import MongoClient
from sqlalchemy import MetaData, select, update, create_engine
from sqlalchemy.engine import Engine
from sqlalchemy.orm import Session, sessionmaker

from config import Config


class DatabaseService:
    """Gestiona acceso a MongoDB y PostgreSQL para estados de resultados."""

    def __init__(self) -> None:
        # Mongo
        self.mongo_client: MongoClient | None = None
        self.db = None
        self.results_collection = None

        # Postgres/SQLAlchemy
        self.engine: Engine | None = None
        self.metadata: MetaData | None = None
        self.results_table = None
        self.result_status_table = None
        self.session: Session | None = None

        # Conectar a MongoDB
        self.mongo_client = MongoClient(Config.MONGO_URI)
        self.db = self.mongo_client.results
        self.results_collection = self.db.results

        # Conectar a PostgreSQL y reflejar metadatos
        self.engine = create_engine(Config.POSTGRES_URI, future=True)
        self.metadata = MetaData()

        # Esperar a que existan las tablas creadas por el API
        required_tables = {"result", "result_status"}
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

        # Validar presencia final de tablas
        if not required_tables.issubset(set(self.metadata.tables.keys())):
            raise RuntimeError(
                "No se encontraron las tablas requeridas 'result' y 'result_status' en Postgres"
            )

        self.results_table = self.metadata.tables["result"]
        self.result_status_table = self.metadata.tables["result_status"]

        # Crear sesión
        SessionLocal = sessionmaker(bind=self.engine, future=True)
        self.session = SessionLocal()

    def update_result_status(self, result_id: int, **kwargs: Any) -> None:
        """Actualiza campos en la tabla result_status para un result dado por collection_id."""
        if self.session is None:
            raise RuntimeError("La sesión de base de datos no está inicializada")

        stmt = select(self.results_table.c.id).where(
            self.results_table.c.collection_id == str(result_id)
        )
        row = self.session.execute(stmt).first()
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
        self.session.execute(upd)
        self.session.commit()

    def get_result_status(self, result_id: int) -> Dict[str, Any]:
        """Obtiene el estado asociado a un result vía collection_id."""
        if self.session is None:
            raise RuntimeError("La sesión de base de datos no está inicializada")

        stmt = select(self.results_table.c.id).where(
            self.results_table.c.collection_id == str(result_id)
        )
        row = self.session.execute(stmt).first()
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
        rs = self.session.execute(st).first()
        if not rs:
            return {"total_files": 0, "total_files_processed": 0, "status": "PENDING"}

        total_files, total_files_processed, status_val = rs
        # status_val puede ser Enum o str según configuración; convertir a str
        status_str = getattr(status_val, "value", status_val)
        return {
            "total_files": total_files or 0,
            "total_files_processed": total_files_processed or 0,
            "status": status_str or "PENDING",
        }

    def close(self) -> None:
        # Cerrar clientes/conexiones de forma segura
        try:
            if self.mongo_client is not None:
                self.mongo_client.close()
        except Exception as exc:
            print(f"[DatabaseService] Error cerrando MongoDB: {exc}")
        try:
            if self.session is not None:
                self.session.close()
        except Exception as exc:
            print(f"[DatabaseService] Error cerrando sesión de Postgres: {exc}")

    def __del__(self) -> None:
        try:
            self.close()
        except Exception:
            pass


