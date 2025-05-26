@echo off
setlocal enabledelayedexpansion

echo ========================================
echo      HERRAMIENTAS DE MANTENIMIENTO
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

:menu_principal
echo Selecciona una opcion:
echo.
echo 1.  Ver estado de servicios
echo 2.  Ver logs en tiempo real
echo 3.  Ver logs de un servicio especifico
echo 4.  Reiniciar un servicio especifico
echo 5.  Reiniciar todos los servicios
echo 6.  Parar todos los servicios
echo 7.  Limpiar logs de Docker
echo 8.  Limpiar imagenes no utilizadas
echo 9.  Ver uso de recursos
echo 10. Backup de base de datos
echo 11. Restaurar base de datos
echo 12. Ejecutar comando en contenedor
echo 13. Salir
echo.
set /p opcion="Ingresa tu opcion (1-13): "

if "%opcion%"=="1" goto ver_estado
if "%opcion%"=="2" goto ver_logs_todos
if "%opcion%"=="3" goto ver_logs_servicio
if "%opcion%"=="4" goto reiniciar_servicio
if "%opcion%"=="5" goto reiniciar_todos
if "%opcion%"=="6" goto parar_todos
if "%opcion%"=="7" goto limpiar_logs
if "%opcion%"=="8" goto limpiar_imagenes
if "%opcion%"=="9" goto ver_recursos
if "%opcion%"=="10" goto backup_db
if "%opcion%"=="11" goto restaurar_db
if "%opcion%"=="12" goto ejecutar_comando
if "%opcion%"=="13" goto salir
echo Opcion invalida
pause
goto menu_principal

:ver_estado
echo.
echo ========================================
echo        ESTADO DE SERVICIOS
echo ========================================
echo.
docker-compose ps
echo.
echo Presiona cualquier tecla para volver al menu...
pause > nul
goto menu_principal

:ver_logs_todos
echo.
echo ========================================
echo      LOGS EN TIEMPO REAL (Ctrl+C para salir)
echo ========================================
echo.
docker-compose logs -f
goto menu_principal

:ver_logs_servicio
echo.
echo Servicios disponibles:
echo - api
echo - converter
echo - worker_cude
echo - worker_rode
echo - worker_legibility
echo - worker_tilde
echo - duplicate
echo - rabbitmq
echo - mongodb
echo - postgres
echo.
set /p servicio="Ingresa el nombre del servicio: "
echo.
echo ========================================
echo      LOGS DE %servicio% (Ctrl+C para salir)
echo ========================================
echo.
docker-compose logs -f %servicio%
goto menu_principal

:reiniciar_servicio
echo.
echo Servicios disponibles:
echo - api
echo - converter
echo - worker_cude
echo - worker_rode
echo - worker_legibility
echo - worker_tilde
echo - duplicate
echo - rabbitmq
echo - mongodb
echo - postgres
echo.
set /p servicio="Ingresa el nombre del servicio a reiniciar: "
echo.
echo Reiniciando %servicio%...
docker-compose restart %servicio%
if %errorlevel% equ 0 (
    echo ✓ Servicio %servicio% reiniciado correctamente
) else (
    echo ✗ Error al reiniciar el servicio %servicio%
)
echo.
pause
goto menu_principal

:reiniciar_todos
echo.
echo Reiniciando todos los servicios...
docker-compose restart
if %errorlevel% equ 0 (
    echo ✓ Todos los servicios reiniciados correctamente
) else (
    echo ✗ Error al reiniciar los servicios
)
echo.
pause
goto menu_principal

:parar_todos
echo.
set /p confirmar="¿Estas seguro de que quieres parar todos los servicios? (s/n): "
if /i "%confirmar%" neq "s" goto menu_principal
echo.
echo Parando todos los servicios...
docker-compose down
if %errorlevel% equ 0 (
    echo ✓ Todos los servicios parados correctamente
) else (
    echo ✗ Error al parar los servicios
)
echo.
pause
goto menu_principal

:limpiar_logs
echo.
echo Limpiando logs de Docker...
docker system prune --volumes -f
if %errorlevel% equ 0 (
    echo ✓ Logs limpiados correctamente
) else (
    echo ✗ Error al limpiar logs
)
echo.
pause
goto menu_principal

:limpiar_imagenes
echo.
echo Limpiando imagenes no utilizadas...
docker image prune -f
if %errorlevel% equ 0 (
    echo ✓ Imagenes limpiadas correctamente
) else (
    echo ✗ Error al limpiar imagenes
)
echo.
pause
goto menu_principal

:ver_recursos
echo.
echo ========================================
echo        USO DE RECURSOS
echo ========================================
echo.
echo Contenedores en ejecucion:
docker stats --no-stream
echo.
echo Uso de disco por imagenes:
docker system df
echo.
pause
goto menu_principal

:backup_db
echo.
echo ========================================
echo        BACKUP DE BASE DE DATOS
echo ========================================
echo.
set fecha=%date:~6,4%-%date:~3,2%-%date:~0,2%_%time:~0,2%-%time:~3,2%-%time:~6,2%
set fecha=%fecha: =0%

echo Creando backup de PostgreSQL...
docker-compose exec -T postgres pg_dump -U user desd > backup_postgres_%fecha%.sql
if %errorlevel% equ 0 (
    echo ✓ Backup de PostgreSQL creado: backup_postgres_%fecha%.sql
) else (
    echo ✗ Error al crear backup de PostgreSQL
)

echo.
echo Creando backup de MongoDB...
docker-compose exec -T mongodb mongodump --host localhost --port 27017 --username root --password example --authenticationDatabase admin --out /tmp/backup_%fecha%
docker-compose exec -T mongodb tar -czf /tmp/backup_mongodb_%fecha%.tar.gz -C /tmp backup_%fecha%
docker cp $(docker-compose ps -q mongodb):/tmp/backup_mongodb_%fecha%.tar.gz ./backup_mongodb_%fecha%.tar.gz
if %errorlevel% equ 0 (
    echo ✓ Backup de MongoDB creado: backup_mongodb_%fecha%.tar.gz
) else (
    echo ✗ Error al crear backup de MongoDB
)
echo.
pause
goto menu_principal

:restaurar_db
echo.
echo ========================================
echo        RESTAURAR BASE DE DATOS
echo ========================================
echo.
echo Archivos de backup disponibles:
dir backup_*.sql backup_*.tar.gz 2>nul
echo.
set /p archivo="Ingresa el nombre del archivo de backup: "
if not exist "%archivo%" (
    echo Error: Archivo no encontrado
    pause
    goto menu_principal
)

if "%archivo:~-4%"==".sql" (
    echo Restaurando PostgreSQL...
    docker-compose exec -T postgres psql -U user -d desd < %archivo%
) else if "%archivo:~-7%"==".tar.gz" (
    echo Restaurando MongoDB...
    docker cp %archivo% $(docker-compose ps -q mongodb):/tmp/
    docker-compose exec mongodb tar -xzf /tmp/%archivo% -C /tmp/
    docker-compose exec mongodb mongorestore --host localhost --port 27017 --username root --password example --authenticationDatabase admin /tmp/backup_*
) else (
    echo Formato de archivo no reconocido
)
echo.
pause
goto menu_principal

:ejecutar_comando
echo.
echo Servicios disponibles:
echo - api
echo - converter
echo - worker_cude
echo - worker_rode
echo - worker_legibility
echo - worker_tilde
echo - duplicate
echo - rabbitmq
echo - mongodb
echo - postgres
echo.
set /p servicio="Ingresa el nombre del servicio: "
set /p comando="Ingresa el comando a ejecutar: "
echo.
echo Ejecutando comando en %servicio%...
docker-compose exec %servicio% %comando%
echo.
pause
goto menu_principal

:salir
echo.
echo Saliendo...
exit /b 0 