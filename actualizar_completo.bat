@echo off
setlocal enabledelayedexpansion

echo ========================================
echo   SCRIPT DE ACTUALIZACION COMPLETA
echo           DESD-API-Rest
echo ========================================
echo.

:: Verificar si estamos en el directorio correcto
if not exist "docker-compose.yml" (
    echo ERROR: No se encontro docker-compose.yml en el directorio actual
    echo Asegurate de ejecutar este script desde la raiz del proyecto
    pause
    exit /b 1
)

echo Selecciona el tipo de actualizacion:
echo.
echo 1. Actualizacion rapida (sin limpiar cache)
echo 2. Actualizacion completa (limpia imagenes y cache)
echo 3. Actualizacion con limpieza de volúmenes (CUIDADO: Borra datos)
echo 4. Solo actualizar codigo (sin Docker)
echo 5. Solo reiniciar servicios
echo.
set /p opcion="Ingresa tu opcion (1-5): "

if "%opcion%"=="1" goto actualizacion_rapida
if "%opcion%"=="2" goto actualizacion_completa
if "%opcion%"=="3" goto actualizacion_con_volumenes
if "%opcion%"=="4" goto solo_codigo
if "%opcion%"=="5" goto solo_reiniciar
echo Opcion invalida
pause
exit /b 1

:actualizacion_rapida
echo.
echo ========================================
echo      ACTUALIZACION RAPIDA
echo ========================================
echo.
goto proceso_basico

:actualizacion_completa
echo.
echo ========================================
echo      ACTUALIZACION COMPLETA
echo ========================================
echo.
set limpiar_imagenes=true
goto proceso_basico

:actualizacion_con_volumenes
echo.
echo ========================================
echo   ACTUALIZACION CON LIMPIEZA TOTAL
echo ========================================
echo.
echo ADVERTENCIA: Esto eliminara todos los datos de la base de datos
echo y archivos almacenados en volúmenes.
echo.
set /p confirmar="¿Estas seguro? Escribe 'SI' para continuar: "
if /i "%confirmar%" neq "SI" (
    echo Operacion cancelada
    pause
    exit /b 1
)
set limpiar_imagenes=true
set limpiar_volumenes=true
goto proceso_basico

:solo_codigo
echo.
echo ========================================
echo      SOLO ACTUALIZAR CODIGO
echo ========================================
echo.
goto actualizar_git

:solo_reiniciar
echo.
echo ========================================
echo      REINICIAR SERVICIOS
echo ========================================
echo.
goto reiniciar_servicios

:proceso_basico
echo [1/7] Deteniendo contenedores Docker...
docker-compose down
if %errorlevel% neq 0 (
    echo ERROR: Fallo al detener los contenedores
    pause
    exit /b 1
)
echo ✓ Contenedores detenidos correctamente
echo.

if defined limpiar_volumenes (
    echo [2/7] Eliminando volúmenes...
    docker-compose down -v
    docker volume prune -f
    echo ✓ Volúmenes eliminados
    echo.
) else (
    echo [2/7] Manteniendo volúmenes...
    echo ✓ Volúmenes preservados
    echo.
)

:actualizar_git
echo [3/7] Verificando estado de Git...
git status
echo.

echo [4/7] Obteniendo ultimos cambios desde Git...
git fetch origin
if %errorlevel% neq 0 (
    echo ERROR: Fallo al hacer fetch desde Git
    pause
    exit /b 1
)
echo ✓ Fetch completado
echo.

echo [5/7] Actualizando rama actual...
git pull origin
if %errorlevel% neq 0 (
    echo ERROR: Fallo al hacer pull desde Git
    echo Puede que tengas cambios locales sin commitear
    echo.
    git status
    echo.
    set /p respuesta="¿Quieres hacer stash de los cambios y continuar? (s/n): "
    if /i "%respuesta%"=="s" (
        git stash
        git pull origin
        echo Cambios guardados en stash. Para recuperarlos usa: git stash pop
    ) else (
        set /p respuesta2="¿Continuar sin actualizar Git? (s/n): "
        if /i "%respuesta2%" neq "s" (
            echo Actualizacion cancelada
            pause
            exit /b 1
        )
    )
) else (
    echo ✓ Codigo actualizado desde Git
)
echo.

if "%opcion%"=="4" goto fin_solo_codigo

if defined limpiar_imagenes (
    echo [6/7] Limpiando imagenes Docker...
    docker system prune -af
    echo ✓ Imagenes y cache limpiados
    echo.
    
    echo [6/7] Construyendo imagenes Docker (sin cache)...
    docker-compose build --no-cache
) else (
    echo [6/7] Construyendo imagenes Docker...
    docker-compose build
)

if %errorlevel% neq 0 (
    echo ERROR: Fallo al construir las imagenes Docker
    pause
    exit /b 1
)
echo ✓ Imagenes construidas correctamente
echo.

:reiniciar_servicios
echo [7/7] Levantando servicios...
docker-compose up -d
if %errorlevel% neq 0 (
    echo ERROR: Fallo al levantar los servicios
    pause
    exit /b 1
)
echo ✓ Servicios levantados correctamente
echo.

echo Esperando a que los servicios esten listos...
timeout /t 10 /nobreak > nul

echo Verificando estado de los servicios...
docker-compose ps
echo.

goto fin_exitoso

:fin_solo_codigo
echo ✓ Codigo actualizado correctamente
echo.
echo Para aplicar los cambios ejecuta:
echo docker-compose restart
echo.
goto fin

:fin_exitoso
echo ========================================
echo        ACTUALIZACION COMPLETADA
echo ========================================
echo.
echo Los servicios estan disponibles en:
echo - API: http://localhost:5000
echo - RabbitMQ Management: http://localhost:15672 (user/password)
echo - MongoDB: localhost:27017
echo - PostgreSQL: localhost:5432
echo.
echo Comandos utiles:
echo - Ver logs: docker-compose logs -f
echo - Ver estado: docker-compose ps
echo - Reiniciar servicio: docker-compose restart [servicio]
echo - Parar todo: docker-compose down
echo.

:fin
pause 