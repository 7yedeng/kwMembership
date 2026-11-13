@echo off
setlocal
py -3 "%~dp0app.py"
if errorlevel 1 pause
