@echo off
setlocal
set "upstream_node=D:\dev\Tools\Node.js\node.exe"
pushd "%~dp0source\automation" || exit /b 1
for %%f in (fill-checkout.js _advance.js _fillcard.js _inspect.js _reload.js _state.js lib\address.js lib\card.js lib\session.js) do (
  "%upstream_node%" --check "%%f"
  if errorlevel 1 goto failed
)
"%upstream_node%" node_modules\playwright\cli.js --version
if errorlevel 1 goto failed
"%upstream_node%" fill-checkout.js > "%~dp0reports\missing-input-recheck.log" 2>&1
set "input_exit=%errorlevel%"
if not "%input_exit%"=="1" goto failed
findstr /C:"--session" "%~dp0reports\missing-input-recheck.log" >nul
if errorlevel 1 goto failed
findstr /C:"--pay-url" "%~dp0reports\missing-input-recheck.log" >nul
if errorlevel 1 goto failed
type "%~dp0reports\missing-input-recheck.log"
echo.
echo PASS: 9 source syntax checks; original entry stops before browser access when no input is supplied.
echo This is an offline check, not a checkout run.
popd
if /i not "%~1"=="--no-pause" pause
exit /b 0
:failed
echo FAIL: inspect the output above and reports directory.
popd
if /i not "%~1"=="--no-pause" pause
exit /b 1
