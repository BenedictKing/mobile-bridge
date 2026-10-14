@echo off
setlocal
pushd "%~dp0"
if not exist "dist\gateway\codex-mobile-gateway.exe" goto missing
"dist\gateway\codex-mobile-gateway.exe" serve --data-dir ".local" %*
set "result=%errorlevel%"
goto end
:missing
echo [mobile-bridge] 未找到网关程序 dist\gateway\codex-mobile-gateway.exe
echo 请先构建: go build -trimpath -ldflags="-s -w" -o dist\gateway\codex-mobile-gateway.exe ./cmd/gateway
set "result=1"
:end
popd
if not "%result%"=="0" pause
