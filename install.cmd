@echo off
REM codehamr Windows installer: fetch the latest release binary and install it
REM into a user writable prefix so admin elevation is never needed.
REM
REM Usage (cmd.exe):
REM   curl -fsSL https://raw.githubusercontent.com/codehamr/codehamr/main/install.cmd -o install.cmd ^&^& install.cmd
REM
REM Usage (PowerShell):
REM   iwr -useb https://raw.githubusercontent.com/codehamr/codehamr/main/install.cmd -outfile install.cmd; .\install.cmd

setlocal enabledelayedexpansion
cls

set "REPO=codehamr/codehamr"

REM Detect arch. PROCESSOR_ARCHITEW6432 catches the 32 bit cmd on 64 bit OS case.
set "arch="
if /I "%PROCESSOR_ARCHITECTURE%"=="AMD64" set "arch=amd64"
if /I "%PROCESSOR_ARCHITECTURE%"=="ARM64" set "arch=arm64"
if /I "%PROCESSOR_ARCHITEW6432%"=="AMD64" set "arch=amd64"
if not defined arch (
  echo codehamr: unsupported arch: %PROCESSOR_ARCHITECTURE% ^(need amd64 or arm64^) 1>&2
  exit /b 1
)

REM Pick install dir. Explicit PREFIX wins. Default lands under LOCALAPPDATA,
REM     the conventional per user install root that never needs admin rights.
if defined PREFIX (
  set "bindir=%PREFIX%\bin"
) else (
  set "bindir=%LOCALAPPDATA%\Programs\codehamr\bin"
)

set "binary=codehamr-windows-%arch%.exe"
set "url=https://github.com/%REPO%/releases/latest/download/%binary%"

echo [codehamr] windows/%arch%

if not exist "%bindir%" mkdir "%bindir%" 2>nul
if not exist "%bindir%" (
  echo codehamr: cannot create %bindir% 1>&2
  exit /b 1
)

REM Download. curl.exe ships with Windows 10 1803+ / 11; fall back to PowerShell
REM     on older systems so a single script covers both.
where curl >nul 2>&1
if %errorlevel%==0 (
  curl -fsSL "%url%" -o "%bindir%\codehamr.exe"
) else (
  powershell -NoProfile -Command "$ProgressPreference='SilentlyContinue'; try { Invoke-WebRequest -UseBasicParsing -Uri '%url%' -OutFile '%bindir%\codehamr.exe' } catch { exit 1 }"
)
if errorlevel 1 (
  echo codehamr: download failed 1>&2
  exit /b 1
)

echo [ok] installed: %bindir%\codehamr.exe

REM Ensure bindir is on the user's persistent PATH.
REM     Read HKCU\Environment\Path directly. Using %PATH% here would be wrong:
REM     it's the merged user+system live value, and writing it back via setx
REM     would clone system entries into the user hive and clobber the
REM     registry's REG_EXPAND_SZ semantics on variables like %SystemRoot%.
set "USERPATH="
for /f "tokens=2*" %%A in ('reg query "HKCU\Environment" /v Path 2^>nul') do set "USERPATH=%%B"

set "needs_setx=1"
if defined USERPATH (
  echo ;!USERPATH!; | findstr /I /C:";%bindir%;" >nul && set "needs_setx="
)

if defined needs_setx (
  if defined USERPATH (
    setx PATH "!USERPATH!;%bindir%" >nul
  ) else (
    setx PATH "%bindir%" >nul
  )
  echo   added %bindir% to user PATH ^(persists for new shells^)
)

REM Patch the LIVE session PATH so cmd.exe users can run codehamr immediately
REM     without opening a new terminal. Propagated past `endlocal` via the standard
REM     `endlocal ^& set` idiom (PATH is captured at parse time, then restored to
REM     the parent scope). This will NOT reach a parent PowerShell process if the
REM     script was launched from one; that's a Windows limitation, not a bug; the
REM     persistent setx above still covers the next terminal.
echo ;%PATH%; | findstr /I /C:";%bindir%;" >nul
if errorlevel 1 set "PATH=%bindir%;%PATH%"

echo.
echo   type 'codehamr' to start hammering
echo   ^(open a new terminal first if 'codehamr' isn't found^)
echo.

endlocal & set "PATH=%PATH%"
