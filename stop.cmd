@echo off
setlocal
pushd "%~dp0"
if not exist "dist\gateway\codex-mobile-gateway.exe" goto missing
"dist\gateway\codex-mobile-gateway.exe" stop --data-dir ".local"
set "result=%errorlevel%"
goto end
:missing
echo [mobile-bridge] 未找到网关程序 dist\gateway\codex-mobile-gateway.exe
set "result=1"
:end
popd
if not "%result%"=="0" pause
