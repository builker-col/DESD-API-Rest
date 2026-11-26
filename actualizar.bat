@echo off
echo ========================================
echo    SCRIPT DE ACTUALIZACION DESD-API
echo ========================================
echo.

:: Verificar si estamos en el directorio correcto
if not exist "docker-compose.yml" (
    echo ERROR: No se encontro docker-compose.yml en el directorio actual
    echo Asegurate de ejecutar este script desde la raiz del proyecto
    pause
    exit /b 1
)

echo [1/6] Deteniendo contenedores Docker...
docker-compose down
if %errorlevel% neq 0 (
    echo ERROR: Fallo al detener los contenedores
    pause
    exit /b 1
)
echo ✓ Contenedores detenidos correctamente
echo.

echo [2/6] Verificando estado de Git...
git status
echo.

echo [3/6] Obteniendo ultimos cambios desde Git...
git fetch origin
if %errorlevel% neq 0 (
    echo ERROR: Fallo al hacer fetch desde Git
    pause
    exit /b 1
)
echo ✓ Fetch completado
echo.

echo [4/6] Actualizando rama actual...
git pull origin
if %errorlevel% neq 0 (
    echo ERROR: Fallo al hacer pull desde Git
    echo Puede que tengas cambios locales sin commitear
    echo.
    set /p respuesta="¿Quieres continuar sin actualizar Git? (s/n): "
    if /i "%respuesta%" neq "s" (
        echo Actualizacion cancelada
        pause
        exit /b 1
    )
) else (
    echo ✓ Codigo actualizado desde Git
)
echo.

echo [5/6] Construyendo imagenes Docker...
docker-compose build --no-cache
if %errorlevel% neq 0 (
    echo ERROR: Fallo al construir las imagenes Docker
    pause
    exit /b 1
)
echo ✓ Imagenes construidas correctamente
echo.

echo [6/6] Levantando servicios...
docker-compose up -d
if %errorlevel% neq 0 (
    echo ERROR: Fallo al levantar los servicios
    pause
    exit /b 1
)
echo ✓ Servicios levantados correctamente
echo.

echo ========================================
echo        ACTUALIZACION COMPLETADA
echo ========================================
echo.
echo Los servicios estan disponibles en:
echo - API: http://localhost:5000
echo - RabbitMQ Management: http://localhost:15672
echo - PostgreSQL: localhost:5432
echo.
echo Para ver los logs en tiempo real ejecuta:
echo docker-compose logs -f
echo.
echo Para ver el estado de los servicios:
echo docker-compose ps
echo.
pause 