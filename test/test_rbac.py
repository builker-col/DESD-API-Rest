import io
import json
import os
import struct
import sys
import time
import urllib.error
import urllib.request
import uuid
import xml.etree.ElementTree as ET
import zipfile
import zlib

BASE_URL = os.getenv("API_BASE_URL", "http://localhost:5000")
ADMIN_USER = os.getenv("API_USERNAME", "admin")
ADMIN_PASS = os.getenv("API_PASSWORD", "admin")


def make_tiny_png() -> bytes:
    """Genera un PNG válido 1x1 en memoria sin dependencias externas."""
    def chunk(ctype: bytes, data: bytes) -> bytes:
        c = ctype + data
        return struct.pack(">I", len(data)) + c + struct.pack(">I", zlib.crc32(c) & 0xFFFFFFFF)

    ihdr = struct.pack(">IIBBBBB", 1, 1, 8, 2, 0, 0, 0)
    raw = b"\x00\xff\x00\x00"
    return (
        b"\x89PNG\r\n\x1a\n"
        + chunk(b"IHDR", ihdr)
        + chunk(b"IDAT", zlib.compress(raw))
        + chunk(b"IEND", b"")
    )


def request_json(method: str, path: str, token: str | None = None, payload: dict | None = None):
    url = f"{BASE_URL}{path}"
    headers = {}
    data = None
    if token:
        headers["Authorization"] = f"Bearer {token}"
    if payload is not None:
        headers["Content-Type"] = "application/json"
        data = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=15) as resp:
            raw = resp.read().decode("utf-8")
            body = json.loads(raw) if raw else None
            return resp.status, body
    except urllib.error.HTTPError as e:
        raw = e.read().decode("utf-8")
        try:
            body = json.loads(raw) if raw else None
        except Exception:
            body = raw
        return e.code, body


def upload_task(token: str, task_id: str, filename: str = "test.png", source_path: str = ""):
    boundary = f"----desd{uuid.uuid4().hex}"
    png_bytes = make_tiny_png()
    parts = [
        f"--{boundary}\r\nContent-Disposition: form-data; name=\"models\"\r\n\r\ncude\r\n".encode(),
        f"--{boundary}\r\nContent-Disposition: form-data; name=\"task_id\"\r\n\r\n{task_id}\r\n".encode(),
    ]
    if source_path:
        parts.append(
            f"--{boundary}\r\nContent-Disposition: form-data; name=\"source_path\"\r\n\r\n{source_path}\r\n".encode()
        )
    parts.extend([
        (
            f"--{boundary}\r\nContent-Disposition: form-data; name=\"files\"; filename=\"{filename}\"\r\n"
            f"Content-Type: image/png\r\n\r\n"
        ).encode()
        + png_bytes
        + b"\r\n",
        f"--{boundary}--\r\n".encode(),
    ])
    body = b"".join(parts)
    req = urllib.request.Request(
        f"{BASE_URL}/api/v1/worker",
        data=body,
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": f"multipart/form-data; boundary={boundary}",
        },
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=20) as resp:
            raw = resp.read().decode("utf-8")
            return resp.status, (json.loads(raw) if raw else None)
    except urllib.error.HTTPError as e:
        raw = e.read().decode("utf-8")
        try:
            return e.code, (json.loads(raw) if raw else None)
        except Exception:
            return e.code, raw


def download_excel(path: str, token: str):
    req = urllib.request.Request(
        f"{BASE_URL}{path}",
        headers={"Authorization": f"Bearer {token}"},
        method="GET",
    )
    try:
        with urllib.request.urlopen(req, timeout=20) as resp:
            return resp.status, resp.headers.get("Content-Type", ""), resp.read()
    except urllib.error.HTTPError as e:
        return e.code, e.headers.get("Content-Type", ""), e.read()


def parse_xlsx_sheet1_rows(xlsx_bytes: bytes) -> tuple[list[str], list[list[str]]]:
    """Lee nombres de hojas y filas de sheet1.xml usando stdlib (zipfile + ElementTree)."""
    ns = {"x": "http://schemas.openxmlformats.org/spreadsheetml/2006/main"}
    with zipfile.ZipFile(io.BytesIO(xlsx_bytes)) as zf:
        wb_xml = ET.fromstring(zf.read("xl/workbook.xml"))
        sheets = [el.attrib.get("name", "") for el in wb_xml.findall(".//x:sheets/x:sheet", ns)]

        shared_strings: list[str] = []
        if "xl/sharedStrings.xml" in zf.namelist():
            ss_xml = ET.fromstring(zf.read("xl/sharedStrings.xml"))
            for si in ss_xml.findall("x:si", ns):
                text = "".join(t.text or "" for t in si.findall(".//x:t", ns))
                shared_strings.append(text)

        sh_xml = ET.fromstring(zf.read("xl/worksheets/sheet1.xml"))
        rows: list[list[str]] = []
        for row_el in sh_xml.findall(".//x:sheetData/x:row", ns):
            vals: list[str] = []
            for c in row_el.findall("x:c", ns):
                ctype = c.attrib.get("t", "")
                if ctype == "inlineStr":
                    val = "".join(t.text or "" for t in c.findall(".//x:t", ns))
                else:
                    v_el = c.find("x:v", ns)
                    raw = v_el.text if v_el is not None and v_el.text is not None else ""
                    if ctype == "s" and raw.isdigit():
                        val = shared_strings[int(raw)]
                    else:
                        val = raw
                vals.append(val)
            rows.append(vals)
        return sheets, rows


def assert_eq(actual, expected, msg: str):
    if actual != expected:
        raise AssertionError(f"FAIL: {msg} | esperado={expected!r}, obtenido={actual!r}")
    print(f"  [OK] {msg}")


def main() -> int:
    suffix = str(int(time.time()))
    created_users: list[int] = []
    created_tasks: list[str] = []
    admin_token = ""

    try:
        # 1. Healthcheck
        code, data = request_json("GET", "/health")
        assert_eq(code, 200, "GET /health devuelve 200")
        assert_eq(data.get("status"), "ok", "Estado de salud es ok")

        # 2. Login admin
        code, admin_data = request_json("POST", "/api/v1/auth", payload={"username": ADMIN_USER, "password": ADMIN_PASS})
        assert_eq(code, 200, "Login admin exitoso")
        admin_token = admin_data["access_token"]
        assert_eq(admin_data.get("role"), "admin", "Login admin devuelve role='admin'")

        # 3. Crear usuarios de prueba (u1, u2, u_inactive, encargado)
        u1_name = f"rbac_u1_{suffix}"
        u2_name = f"rbac_u2_{suffix}"
        ui_name = f"rbac_inact_{suffix}"
        enc_name = f"rbac_enc_{suffix}"

        code, u1 = request_json(
            "POST", "/api/v1/admin/user", token=admin_token,
            payload={"username": u1_name, "password": "secret", "role": "user"},
        )
        assert_eq(code, 201, "Admin crea usuario u1 (role=user)")
        created_users.append(u1["id"])
        assert_eq(u1["role"], "user", "u1 tiene role='user'")
        assert_eq(u1["is_admin"], False, "u1 tiene is_admin=False")

        code, u2 = request_json(
            "POST", "/api/v1/admin/user", token=admin_token,
            payload={"username": u2_name, "password": "secret", "role": "user"},
        )
        assert_eq(code, 201, "Admin crea usuario u2 (role=user)")
        created_users.append(u2["id"])

        code, ui = request_json(
            "POST", "/api/v1/admin/user", token=admin_token,
            payload={"username": ui_name, "password": "secret", "role": "user", "is_active": False},
        )
        assert_eq(code, 201, "Admin crea usuario inactivo")
        created_users.append(ui["id"])

        code, _ = request_json("POST", "/api/v1/auth", payload={"username": ui_name, "password": "secret"})
        assert_eq(code, 403, "Usuario inactivo bloqueado en login (403)")

        code, enc = request_json(
            "POST", "/api/v1/admin/user", token=admin_token,
            payload={
                "username": enc_name,
                "password": "secret",
                "role": "encargado",
                "assigned_user_ids": [u1["id"]],
            },
        )
        assert_eq(code, 201, "Admin crea encargado con u1 asignado")
        created_users.append(enc["id"])
        assert_eq(enc["role"], "encargado", "enc tiene role='encargado'")
        assert_eq(enc.get("assigned_user_ids"), [u1["id"]], "enc devuelve assigned_user_ids=[u1.id]")

        # 4. Login de u1, u2 y encargado
        _, d1 = request_json("POST", "/api/v1/auth", payload={"username": u1_name, "password": "secret"})
        u1_token = d1["access_token"]
        _, d2 = request_json("POST", "/api/v1/auth", payload={"username": u2_name, "password": "secret"})
        u2_token = d2["access_token"]
        code, d_enc = request_json("POST", "/api/v1/auth", payload={"username": enc_name, "password": "secret"})
        assert_eq(code, 200, "Login encargado exitoso")
        assert_eq(d_enc.get("role"), "encargado", "Login encargado devuelve role='encargado'")
        enc_token = d_enc["access_token"]

        # Verificar /api/v1/encargado/users
        code, assigned_list = request_json("GET", "/api/v1/encargado/users", token=enc_token)
        assert_eq(code, 200, "Encargado consulta sus usuarios asignados")
        assert_eq([u["id"] for u in assigned_list], [u1["id"]], "Encargado solo ve a u1 asignado")

        code, _ = request_json("GET", "/api/v1/encargado/users", token=u1_token)
        assert_eq(code, 403, "Usuario regular no puede acceder a /api/v1/encargado/users (403)")

        # 5. Subir tareas reales para u1, u2 y encargado
        t_u1 = f"rbac_task_u1_{suffix}"
        t_u2 = f"rbac_task_u2_{suffix}"
        t_enc = f"rbac_task_enc_{suffix}"

        for tok, tid, label, fname, spath in [
            (u1_token, t_u1, "u1", "doc_u1.png", "/lotes/u1"),
            (u2_token, t_u2, "u2", "doc_u2.png", "/lotes/u2"),
            (enc_token, t_enc, "enc", "doc_enc.png", "/lotes/enc"),
        ]:
            code, resp = upload_task(tok, tid, filename=fname, source_path=spath)
            assert_eq(code, 202, f"Upload de tarea {label} ({tid}) aceptado (202): {resp}")
            created_tasks.append(tid)

        # 6. Verificar visibilidad de u1 (solo sus propias tareas)
        code, data = request_json("GET", "/api/v1/task", token=u1_token)
        assert_eq(code, 200, "u1 lista tareas")
        assert_eq({t["collection_id"] for t in data["results"]}, {t_u1}, "u1 solo ve su propia tarea")

        code, data = request_json("GET", f"/api/v1/task/{t_u1}", token=u1_token)
        assert_eq(code, 200, "u1 puede ver detalle de su propia tarea")
        assert_eq(data.get("username"), u1_name, "Detalle de tarea incluye username del propietario")

        code, _ = request_json("GET", f"/api/v1/task/{t_u2}", token=u1_token)
        assert_eq(code, 403, "u1 NO puede ver detalle de tarea de u2 (403)")

        code, _ = request_json("GET", f"/api/v1/export/{t_u2}", token=u1_token)
        assert_eq(code, 403, "u1 NO puede exportar tarea de u2 (403)")

        code, _ = request_json("GET", f"/api/v1/task?user_id={u2['id']}", token=u1_token)
        assert_eq(code, 403, "u1 NO puede filtrar por user_id de u2 (403)")

        # 7. Verificar visibilidad de encargado (propias + u1, NO u2)
        code, data = request_json("GET", "/api/v1/task", token=enc_token)
        assert_eq(code, 200, "encargado lista tareas")
        assert_eq({t["collection_id"] for t in data["results"]}, {t_enc, t_u1}, "encargado ve su tarea y la de u1, pero NO la de u2")

        code, data = request_json("GET", f"/api/v1/task?user_id={u1['id']}", token=enc_token)
        assert_eq(code, 200, "encargado puede filtrar por usuario asignado u1")
        assert_eq({t["collection_id"] for t in data["results"]}, {t_u1}, "Filtro u1 devuelve solo t_u1")

        code, _ = request_json("GET", f"/api/v1/task?user_id={u2['id']}", token=enc_token)
        assert_eq(code, 403, "encargado NO puede filtrar por usuario NO asignado u2 (403)")

        code, _ = request_json("GET", f"/api/v1/task/{t_u1}", token=enc_token)
        assert_eq(code, 200, "encargado puede ver detalle de tarea de u1")

        code, _ = request_json("GET", f"/api/v1/task/{t_u2}", token=enc_token)
        assert_eq(code, 403, "encargado NO puede ver detalle de tarea de u2 (403)")

        code, _ = request_json("GET", f"/api/v1/export/{t_u2}", token=enc_token)
        assert_eq(code, 403, "encargado NO puede exportar tarea de u2 (403)")

        code, _ = request_json("DELETE", f"/api/v1/task/{t_u1}", token=enc_token)
        assert_eq(code, 403, "encargado NO puede eliminar tarea de u1 (403)")

        # 8. Admin asigna u2 al encargado y verificamos visibilidad dinámica
        code, assigned = request_json("POST", f"/api/v1/admin/encargado/{enc['id']}/users/{u2['id']}", token=admin_token)
        assert_eq(code, 200, "Admin asigna u2 al encargado")
        assert_eq({u["id"] for u in assigned}, {u1["id"], u2["id"]}, "Lista asignada incluye u1 y u2")

        code, data = request_json("GET", "/api/v1/task", token=enc_token)
        assert_eq({t["collection_id"] for t in data["results"]}, {t_enc, t_u1, t_u2}, "encargado ahora ve t_enc, t_u1 y t_u2")

        code, _ = request_json("GET", f"/api/v1/task/{t_u2}", token=enc_token)
        assert_eq(code, 200, "encargado ahora puede ver detalle de t_u2")

        # 9. Admin desasigna u2 del encargado
        code, _ = request_json("DELETE", f"/api/v1/admin/encargado/{enc['id']}/users/{u2['id']}", token=admin_token)
        assert_eq(code, 204, "Admin desasigna u2 del encargado (204)")

        code, _ = request_json("GET", f"/api/v1/task/{t_u2}", token=enc_token)
        assert_eq(code, 403, "encargado pierde acceso a t_u2 tras desasignación (403)")

        # 10. Verificar visibilidad de admin (ve todas las tareas)
        code, data = request_json("GET", "/api/v1/task", token=admin_token)
        assert_eq(code, 200, "admin lista todas las tareas en /api/v1/task")
        admin_ids = {t["collection_id"] for t in data["results"]}
        assert_eq({t_u1, t_u2, t_enc}.issubset(admin_ids), True, "admin ve t_u1, t_u2 y t_enc")

        code, data = request_json("GET", f"/api/v1/task?user_id={u2['id']}", token=admin_token)
        assert_eq(code, 200, "admin puede filtrar por cualquier user_id")
        assert_eq({t["collection_id"] for t in data["results"]}, {t_u2}, "admin filtrado por u2 devuelve solo t_u2")

        # 11. Pruebas de Reporte Excel (/api/v1/report/excel)
        code, _, _ = download_excel("/api/v1/report/excel", u1_token)
        assert_eq(code, 403, "Usuario regular NO puede descargar reporte Excel (403)")

        # Encargado (actualmente solo tiene a u1 asignado): descarga todos sus archivos + u1
        code, ctype, raw_xlsx = download_excel("/api/v1/report/excel", enc_token)
        assert_eq(code, 200, "Encargado descarga reporte Excel (200)")
        assert_eq("spreadsheetml.sheet" in ctype, True, "Content-Type es Excel .xlsx")
        sheets, rows = parse_xlsx_sheet1_rows(raw_xlsx)
        assert_eq(sheets, ["Archivos Procesados", "Resumen por Usuario"], "Excel contiene ambas hojas esperadas")
        enc_report_tasks = {r[2] for r in rows[1:] if len(r) >= 5}
        assert_eq(enc_report_tasks, {t_enc, t_u1}, "Excel del encargado incluye solo t_enc y t_u1")
        u1_row = [r for r in rows[1:] if len(r) >= 5 and r[2] == t_u1][0]
        assert_eq(u1_row[0], u1_name, "Fila Excel incluye username correcto")
        assert_eq(u1_row[3], "doc_u1.png", "Fila Excel incluye nombre de archivo correcto")
        assert_eq(u1_row[4], "/lotes/u1/doc_u1.png", "Fila Excel incluye ruta correcta")

        # Encargado filtra por u1 (permitido) y por u2 (prohibido)
        code, _, raw_xlsx = download_excel(f"/api/v1/report/excel?user_id={u1['id']}", enc_token)
        assert_eq(code, 200, "Encargado descarga Excel filtrado por u1")
        _, rows = parse_xlsx_sheet1_rows(raw_xlsx)
        assert_eq({r[2] for r in rows[1:]}, {t_u1}, "Excel del encargado filtrado por u1 contiene solo t_u1")

        code, _, _ = download_excel(f"/api/v1/report/excel?user_id={u2['id']}", enc_token)
        assert_eq(code, 403, "Encargado NO puede descargar Excel de usuario no asignado u2 (403)")

        # Admin descarga Excel de todos los usuarios y de usuarios seleccionados (?user_ids=)
        code, _, raw_xlsx = download_excel("/api/v1/report/excel", admin_token)
        assert_eq(code, 200, "Admin descarga Excel global (200)")
        _, rows = parse_xlsx_sheet1_rows(raw_xlsx)
        admin_report_tasks = {r[2] for r in rows[1:]}
        assert_eq({t_u1, t_u2, t_enc}.issubset(admin_report_tasks), True, "Excel global del admin incluye t_u1, t_u2 y t_enc")

        code, _, raw_xlsx = download_excel(f"/api/v1/report/excel?user_ids={u1['id']},{u2['id']}", admin_token)
        assert_eq(code, 200, "Admin descarga Excel de usuarios seleccionados (u1, u2)")
        _, rows = parse_xlsx_sheet1_rows(raw_xlsx)
        assert_eq({r[2] for r in rows[1:]}, {t_u1, t_u2}, "Excel filtrado por user_ids=u1,u2 contiene solo t_u1 y t_u2")

        print("\nTODAS LAS PRUEBAS E2E DE USUARIOS, ROLES Y REPORTE EXCEL PASARON EXITOSAMENTE.")
        return 0
    finally:
        if admin_token:
            for tid in created_tasks:
                try:
                    request_json("DELETE", f"/api/v1/admin/task/{tid}", token=admin_token)
                except Exception:
                    pass
            for uid in created_users:
                try:
                    request_json("DELETE", f"/api/v1/admin/user/{uid}", token=admin_token)
                except Exception:
                    pass


if __name__ == "__main__":
    sys.exit(main())
