@echo off
setlocal
python -c "import tkinter" >nul 2>&1
if not errorlevel 1 (
    python "%~dp0app.py"
    goto finished
)
py -3 -c "import tkinter" >nul 2>&1
if not errorlevel 1 (
    py -3 "%~dp0app.py"
    goto finished
)
echo Python 3 with Tkinter was not found. Check your Python installation.
pause
exit /b 1
:finished
if errorlevel 1 pause
