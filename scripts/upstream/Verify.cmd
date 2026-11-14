@echo off
setlocal
"%~dp0venv\Scripts\python.exe" "%~dp0Verify-Runtime.py" --root "%~dp0."
set "verify_exit=%errorlevel%"
echo.
echo This checks the original source and runtime only. It does not start a worker or checkout.
if /i not "%~1"=="--no-pause" pause
exit /b %verify_exit%
