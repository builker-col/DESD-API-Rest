from flask_jwt_extended import jwt_required
from flask_restful import Resource
from flask import send_file, abort, request
from app.services import ExportService

class ExportResource(Resource):
    """
    GET /api/v1/export/<task_id>
    Descripción: Exporta un CSV con resultados de la colección Mongo asociada al task.
    Auth: JWT requerido
    Query params:
      - formato (opcional): normal | no_tecnico (por defecto: normal)
    Respuestas:
      - 200: CSV descargable
      - 404: Task ID no encontrado o colección vacía
    """
    @jwt_required()
    def get(self, task_id: str):
        export_service = ExportService()
        formato = request.args.get('formato', 'normal')
        filename = export_service.export_collection(task_id, formato=formato)
        
        if not filename:
            abort(404, "Task ID not found")
            
        return send_file(filename, as_attachment=True, mimetype='text/csv')