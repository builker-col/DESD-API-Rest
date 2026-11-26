import os
import sys
import json
import requests


def main() -> int:
    base_url = os.getenv("API_BASE_URL", "http://localhost:5000")
    url = f"{base_url}/api/v1/auth"

    username = os.getenv("API_USERNAME", "admin")
    password = os.getenv("API_PASSWORD", "admin")

    headers = {"Content-Type": "application/json"}
    payload = {"username": username, "password": password}

    try:
        response = requests.post(url, headers=headers, json=payload, timeout=10)
    except requests.RequestException as exc:
        print(f"ERROR: No se pudo conectar con la API: {exc}")
        return 2

    if response.status_code != 200:
        print(f"ERROR: Respuesta HTTP inesperada: {response.status_code}")
        try:
            print(response.text)
        except Exception:
            pass
        return 1

    try:
        data = response.json()
    except json.JSONDecodeError:
        print("ERROR: La respuesta no es JSON válido")
        return 1

    token = data.get("access_token")
    if not token:
        print("ERROR: No se encontró 'access_token' en la respuesta")
        return 1

    print("Login OK. access_token recibido.")
    return 0


if __name__ == "__main__":
    sys.exit(main())


