@echo off
setlocal enabledelayedexpansion
title mrp Deploy

REM ============================================================
REM  mrp one-click deployment script
REM  Two-level menu: first choose a deploy target (Windows / Android / HarmonyOS),
REM  then choose the operation: Windows = deploy / stop, device = install / start /
REM  stop / uninstall.
REM  The CA pair is fixed and embedded in this script so openssl is never required;
REM  the private key is shared by design and deployment works out of the box.
REM ============================================================

REM Config constants (edit these to change defaults)
set "WORKDIR=%~dp0"
set "EXE=mrp-windows-amd64.exe"
set "CRT=ca.crt"
set "KEY=ca.key"
set "PORT=4000"
set "CA_HASH=d6cd00d8"
set "DEV_BIN=mrp-linux-arm64"
set "DEV_CFG=config.yaml"

REM Elevate dispatch: the elevated instance gets a subroutine name argument,
REM runs it, then pauses to show the result before exiting
if not "%~1"=="" (
    call :%~1
    echo.
    pause
    exit /b
)

REM ============================================================
REM Main loop: probe device tools, then render the main menu
REM ============================================================
:main
set "TOOL=adb"
call :probeDevReady
set "ANDROID_DESC=!TOOL_DESC!"
set "TOOL=hdc"
call :probeDevReady
set "HARMONY_DESC=!TOOL_DESC!"
call :mainMenu
if "!QUIT!"=="1" goto :end
goto :main

REM ============================================================
REM Main menu: choose a deploy target
REM ============================================================
:mainMenu
cls
echo ==============================
echo     mrp Deploy
echo ==============================
echo [1] Windows
echo [2] Android       !ANDROID_DESC!
echo [3] HarmonyOS     !HARMONY_DESC!
echo ------------------------------
echo [0] Exit
set "QUIT="
set "CHOICE="
set /p "CHOICE=Select [0-3]: "
if "!CHOICE!"=="0" set "QUIT=1"
if "!CHOICE!"=="1" call :subWindows
if "!CHOICE!"=="2" call :subAndroid
if "!CHOICE!"=="3" call :subHarmony
exit /b

REM ============================================================
REM Device tool probe: is !TOOL! installed and is a device connected
REM   Outputs TOOL_READY (0/1) and TOOL_DESC (install/connection status)
REM ============================================================
:probeDevReady
set "TOOL_READY=0"
set "TOOL_DESC=Not installed"
where !TOOL! >nul 2>&1
if not "!errorlevel!"=="0" exit /b
set "TOOL_DESC=Installed, no device connected"
if /i "!TOOL!"=="adb" (
    for /f "skip=1 tokens=2" %%i in ('adb devices 2^>nul') do if "!TOOL_READY!"=="0" if "%%i"=="device" set "TOOL_READY=1"
) else (
    for /f "tokens=1" %%i in ('hdc list targets 2^>nul') do if "!TOOL_READY!"=="0" if not "%%i"=="[Empty]" if not "%%i"=="Connect" set "TOOL_READY=1"
)
if "!TOOL_READY!"=="1" set "TOOL_DESC=Device connected"
exit /b

REM ============================================================
REM Windows submenu
REM ============================================================
:subWindows
:winLoop
call :probeWin
cls
echo ==============================
echo     Windows Deploy
echo ==============================
echo [A] Files          !A_DESC!
echo [B] CA certificate !B_DESC!
echo [C] mrp process    !C_DESC!
echo [D] System proxy   !D_DESC!
echo ------------------------------
echo [1] One-click deploy
echo [2] Stop - kill process + clear proxy
echo [0] Back
set "CHOICE="
set /p "CHOICE=Select: "
if "!CHOICE!"=="1" (call :winDeploy & echo. & pause & goto :winLoop)
if "!CHOICE!"=="2" (call :winStop & echo. & pause & goto :winLoop)
if "!CHOICE!"=="0" exit /b
goto :winLoop

REM Windows status probe and descriptions
:probeWin
call :checkFiles
call :probeCert
call :probeRun
call :probeProxy
if "!A_STATE!"=="OK" (set "A_DESC=OK") else (set "A_DESC=Missing!MISS!")
if "!B_STATE!"=="NONE" (set "B_DESC=Not imported") else (set "B_DESC=Imported [!B_STATE!]")
if "!C_STATE!"=="RUN" (set "C_DESC=Running") else (set "C_DESC=Stopped")
if "!D_STATE!"=="ON" (set "D_DESC=Configured") else (set "D_DESC=Not set")
exit /b

REM ============================================================
REM Windows one-click deploy: prepare files -> import CA -> start mrp -> set proxy
REM   Each step probes first and skips if already done
REM ============================================================
:winDeploy
echo.
echo [1/4] Prepare files
call :ensureCA
if "!EXE_MISSING!"=="1" call :fetchFile %EXE%
call :checkFiles
if "!A_STATE!"=="BAD" (echo Files still missing, cannot continue. & exit /b 1)

echo [2/4] Import CA certificate
call :probeCert
if "!B_STATE!"=="NONE" (certutil -user -addstore Root "%WORKDIR%\%CRT%") else (echo Already imported - !B_STATE!, skipped.)

echo [3/4] Start mrp
call :probeRun
if "!C_STATE!"=="RUN" (echo mrp already running, skipped.) else (start "mrp" /D "%WORKDIR%" "%WORKDIR%\%EXE%" & echo mrp started in a new window.)

echo [4/4] Configure system proxy
call :probeProxy
if "!D_STATE!"=="ON" (
    echo System proxy already set, skipped.
) else (
    call :runElevated doProxySet
    call :probeProxy
    if "!D_STATE!"=="ON" (echo System proxy set to 127.0.0.1:!PORT!.) else (echo Warning: proxy not applied; UAC may have been cancelled or the command failed.)
)
echo Windows deploy finished.
exit /b

REM Windows stop: close process and clear system proxy
:winStop
echo.
echo Stopping mrp process...
call :probeRun
if "!C_STATE!"=="RUN" (taskkill /im "%EXE%" /f) else (echo mrp not running, skipped.)
echo Clearing system proxy...
call :runElevated doProxyReset
call :probeProxy
if "!D_STATE!"=="ON" echo Warning: system proxy clear failed.
echo Stopped.
exit /b

REM ============================================================
REM Device submenu: shared by Android / HarmonyOS, DEV_TOOL differs
REM ============================================================
:subAndroid
set "DEV_TOOL=adb"
set "DEV_NAME=Android"
call :devMenu
exit /b

:subHarmony
set "DEV_TOOL=hdc"
set "DEV_NAME=HarmonyOS"
call :devMenu
exit /b

:devMenu
:devLoop
set "TOOL=!DEV_TOOL!"
call :probeDevReady
call :setDevPaths
set "DEV_FILE_DESC=Not ready"
set "DEV_RUN_DESC=Not running"
set "DEV_PROXY_DESC=Unknown"
if "!TOOL_READY!"=="1" call :probeDevState
cls
echo ==============================
echo     !DEV_NAME! Deploy
echo ==============================
echo Device tool: !DEV_TOOL! - !TOOL_DESC!
echo ------------------------------
if "!TOOL_READY!"=="1" (
    echo [A] Device files    !DEV_FILE_DESC!
    echo [B] Device process  !DEV_RUN_DESC!
    echo [C] Device proxy    !DEV_PROXY_DESC!
) else (
    echo Device not connected; check USB debugging and connection.
)
echo ------------------------------
echo [1] Install   push files + install CA
echo [2] Start     run mrp + set proxy
echo [3] Stop      kill mrp + clear proxy
echo [4] Uninstall stop + remove CA + files
echo [0] Back
set "CHOICE="
set /p "CHOICE=Select: "
if "!CHOICE!"=="0" exit /b
if "!TOOL_READY!"=="0" (echo Device not connected, cannot proceed. & timeout /t 2 >nul & goto :devLoop)
if "!CHOICE!"=="1" (call :devInstall & echo. & pause & goto :devLoop)
if "!CHOICE!"=="2" (call :devStart & echo. & pause & goto :devLoop)
if "!CHOICE!"=="3" (call :devStop & echo. & pause & goto :devLoop)
if "!CHOICE!"=="4" (call :devUninstall & echo. & pause & goto :devLoop)
goto :devLoop

REM Device paths and push command: adb vs hdc tmp dir, system cert dir, push verb
:setDevPaths
if /i "!DEV_TOOL!"=="adb" (
    set "DEV_REMOTE=/data/local/tmp"
    set "DEV_CERTS=/system/etc/security/cacerts"
    set "PUSHCMD=push"
) else (
    set "DEV_REMOTE=data/local/mrp"
    set "DEV_CERTS=/etc/security/certificates"
    set "PUSHCMD=file send"
)
exit /b

REM Device status lazy probe: file / process / proxy
:probeDevState
!DEV_TOOL! shell test -f !DEV_REMOTE!/!DEV_BIN! >nul 2>&1
if "!errorlevel!"=="0" set "DEV_FILE_DESC=Ready"
!DEV_TOOL! shell pidof !DEV_BIN! >nul 2>&1
if "!errorlevel!"=="0" set "DEV_RUN_DESC=Running"
if /i not "!DEV_TOOL!"=="adb" exit /b
set "PROXY_VALUE="
for /f "delims=" %%i in ('!DEV_TOOL! shell settings get global http_proxy 2^>nul') do set "PROXY_VALUE=%%i"
if "!PROXY_VALUE!"=="127.0.0.1:!PORT!" set "DEV_PROXY_DESC=Configured"
exit /b

REM ============================================================
REM Device install: prepare files -> push -> install CA (does not start)
REM ============================================================
:devInstall
echo.
echo [1/3] Prepare files
call :ensureCA
call :checkDevFiles
if "!DEV_BIN_MISSING!"=="1" call :fetchFile %DEV_BIN%
call :checkDevFiles
if "!DEV_BIN_MISSING!"=="1" (echo Missing %DEV_BIN%, cannot continue. & exit /b 1)
if "!CRT_MISSING!"=="1" (echo Missing CA certificate, cannot continue. & exit /b 1)
if "!KEY_MISSING!"=="1" (echo Missing CA private key, cannot continue. & exit /b 1)

echo [2/3] Push files to device
call :devPush

echo [3/3] Install CA to device system store
call :devCert
echo Device install finished.
exit /b

REM Device stop: end mrp process and clear global proxy
:devStop
echo.
echo Stopping device mrp process...
!DEV_TOOL! shell pkill -f !DEV_BIN! >nul 2>&1
echo Clearing device global proxy...
call :devProxyCfg off
echo Stopped.
exit /b

REM Device start: run mrp and set global proxy
:devStart
echo.
call :devRun
call :devProxyCfg on
echo Device start finished.
exit /b

REM Device uninstall: stop, clear proxy, then remove CA and device files
:devUninstall
echo.
call :devStop
call :devRemoveCA
call :devRemoveFiles
echo Device uninstall finished.
exit /b

REM ============================================================
REM File and status probe subroutines
REM ============================================================

REM Ensure local CA and binaries exist: write embedded CA if missing, prompt to copy binary
:ensureCA
call :checkFiles
set "NEED_CA=0"
if "!CRT_MISSING!"=="1" set "NEED_CA=1"
if "!KEY_MISSING!"=="1" set "NEED_CA=1"
if "!NEED_CA!"=="1" call :writeEmbeddedCA
call :checkFiles
exit /b

REM When a binary is missing, ask the user for its directory and copy it in - %1 = filename
:fetchFile
echo %~1 not found. Enter the directory containing it - e.g. your GitHub Releases download folder:
set "SRC="
set /p "SRC=Directory: "
if not defined SRC exit /b
set SRC=!SRC:"=!
if exist "!SRC!\%~1" (copy /y "!SRC!\%~1" "%WORKDIR%\" >nul & echo Copied %~1.) else (echo %~1 not found in that directory.)
exit /b

REM Whether EXE / ca.crt / ca.key are in place
:checkFiles
set "A_STATE=OK"
set "EXE_MISSING=0"
set "CRT_MISSING=0"
set "KEY_MISSING=0"
set "MISS="
if not exist "%WORKDIR%\%EXE%" (set "A_STATE=BAD" & set "EXE_MISSING=1" & set "MISS=!MISS! %EXE%")
if not exist "%WORKDIR%\%CRT%" (set "A_STATE=BAD" & set "CRT_MISSING=1" & set "MISS=!MISS! %CRT%")
if not exist "%WORKDIR%\%KEY%" (set "A_STATE=BAD" & set "KEY_MISSING=1" & set "MISS=!MISS! %KEY%")
exit /b

REM Whether CA is imported into the user/machine Root store
:probeCert
set "B_STATE=NONE"
set "CN="
if not exist "%WORKDIR%\%CRT%" exit /b
for /f "tokens=2 delims==" %%i in ('certutil -dump "%WORKDIR%\%CRT%" ^| findstr /i /c:"CN="') do if not defined CN set "CN=%%i"
if not defined CN exit /b
certutil -store Root 2>nul | findstr /i /c:"CN=!CN!" >nul
if "!errorlevel!"=="0" set "B_STATE=MACHINE"
if "!B_STATE!"=="NONE" (
    certutil -user -store Root 2>nul | findstr /i /c:"CN=!CN!" >nul
    if "!errorlevel!"=="0" set "B_STATE=USER"
)
exit /b

REM Whether the mrp process is running
:probeRun
set "C_STATE=STOP"
tasklist /fi "imagename eq %EXE%" /nh 2>nul | find /i "%EXE%" >nul && set "C_STATE=RUN"
exit /b

REM Whether WinHTTP system proxy points to the local port
:probeProxy
set "D_STATE=OFF"
netsh winhttp show proxy 2>nul | find /i "127.0.0.1:%PORT%" >nul && set "D_STATE=ON"
exit /b

REM Whether the local device binary is present
:checkDevFiles
set "DEV_BIN_MISSING=0"
if not exist "%WORKDIR%\%DEV_BIN%" set "DEV_BIN_MISSING=1"
exit /b

REM ============================================================
REM Device operation subroutines
REM ============================================================

REM Push binary, config and CA to device - PUSHCMD is push / file send
:devPush
echo Pushing files to device [!DEV_TOOL!]...
if /i not "!DEV_TOOL!"=="adb" !DEV_TOOL! shell mkdir -p !DEV_REMOTE!
!DEV_TOOL! !PUSHCMD! "%WORKDIR%\%DEV_BIN%" !DEV_REMOTE!
if exist "%WORKDIR%\%DEV_CFG%" !DEV_TOOL! !PUSHCMD! "%WORKDIR%\%DEV_CFG%" !DEV_REMOTE!
!DEV_TOOL! !PUSHCMD! "%WORKDIR%\%CRT%" !DEV_REMOTE!
!DEV_TOOL! !PUSHCMD! "%WORKDIR%\%KEY%" !DEV_REMOTE!
!DEV_TOOL! shell chmod +x !DEV_REMOTE!/!DEV_BIN!
echo Files pushed.
exit /b

REM Install CA to device system cert dir - named by the embedded CA hash
:devCert
echo Installing CA certificate to device system store...
set "HASH=!CA_HASH!"
echo Cert hash: !HASH!.0
if "!DEV_TOOL!"=="adb" (
    !DEV_TOOL! root
    timeout /t 3 >nul
    !DEV_TOOL! wait-for-device
    !DEV_TOOL! remount
    !DEV_TOOL! push "%WORKDIR%\%CRT%" !DEV_CERTS!/!HASH!.0
    !DEV_TOOL! shell chmod 644 !DEV_CERTS!/!HASH!.0
) else (
    copy /y "%WORKDIR%\%CRT%" "%TEMP%\!HASH!.0" >nul
    !DEV_TOOL! file send "%TEMP%\!HASH!.0" !DEV_CERTS!
)
!DEV_TOOL! shell test -f !DEV_CERTS!/!HASH!.0 >nul 2>&1
if "!errorlevel!"=="0" (echo CA installed as !DEV_CERTS!/!HASH!.0.) else (echo Warning: cert install may have failed; ensure the device is rooted or in developer mode.)
exit /b

REM Start mrp on device - new window keeps logs; skip only if confirmed running
:devRun
!DEV_TOOL! shell pidof !DEV_BIN! >nul 2>&1
if not "!errorlevel!"=="0" goto :devRunLaunch
REM Re-check after 1s so the probe command itself is not mistaken for mrp
timeout /t 1 >nul
!DEV_TOOL! shell pidof !DEV_BIN! >nul 2>&1
if "!errorlevel!"=="0" (
    echo mrp already running on device, skipping.
    exit /b
)
:devRunLaunch
echo Starting mrp on device...
start "mrp device" cmd /k !DEV_TOOL! shell "cd !DEV_REMOTE!; ./!DEV_BIN!"
set "DEV_RUN_WAIT=0"
:devRunWait
timeout /t 1 >nul
!DEV_TOOL! shell pidof !DEV_BIN! >nul 2>&1
if "!errorlevel!"=="0" (
    echo mrp is now running on device - Ctrl+C in the new window to stop.
    exit /b
)
set /a "DEV_RUN_WAIT+=1"
if !DEV_RUN_WAIT! lss 5 goto :devRunWait
echo Warning: mrp did not start within 5 seconds. Check the new window for errors.
exit /b

REM Device global HTTP proxy: on sets the port, off clears - adb/hdc clear values differ
:devProxyCfg
set "V_ADB=127.0.0.1:!PORT!"
set "V_HDC=127.0.0.1:!PORT!"
if /i "%~1"=="off" (set "V_ADB=:0" & set "V_HDC=0")
if /i "!DEV_TOOL!"=="adb" (
    !DEV_TOOL! shell settings put global http_proxy !V_ADB!
) else (
    !DEV_TOOL! shell network-cfg set http_proxy !V_HDC!
)
if not "!errorlevel!"=="0" (echo Warning: device proxy set failed; check device connection. & exit /b)
if /i "%~1"=="off" (echo Device global proxy cleared.) else (echo Device global proxy set to 127.0.0.1:!PORT!.)
exit /b

REM Remove CA from device system cert dir
:devRemoveCA
echo Removing CA certificate from device...
if /i "!DEV_TOOL!"=="adb" (
    !DEV_TOOL! root
    timeout /t 3 >nul
    !DEV_TOOL! wait-for-device
    !DEV_TOOL! remount
)
!DEV_TOOL! shell rm -f !DEV_CERTS!/!CA_HASH!.0
exit /b

REM Remove mrp binary, config and CA from device
:devRemoveFiles
echo Removing device files...
!DEV_TOOL! shell rm -f !DEV_REMOTE!/!DEV_BIN!
!DEV_TOOL! shell rm -f !DEV_REMOTE!/!CRT!
!DEV_TOOL! shell rm -f !DEV_REMOTE!/!KEY!
if exist "%WORKDIR%\%DEV_CFG%" !DEV_TOOL! shell rm -f !DEV_REMOTE!/!DEV_CFG!
exit /b

REM ============================================================
REM Common utility subroutines
REM ============================================================

REM Admin-required operations: run directly if already admin, else elevate - %1 = subroutine name
:runElevated
call :isAdmin
if "!ADMIN!"=="1" (call :%~1) else (call :elevate %~1)
exit /b

REM Write the fixed embedded CA pair - only when ca.crt / ca.key are missing
:writeEmbeddedCA
if exist "%WORKDIR%\%CRT%" goto :writeCAKey
>"%WORKDIR%\%CRT%" echo -----BEGIN CERTIFICATE-----
>>"%WORKDIR%\%CRT%" echo MIIBmDCCAT2gAwIBAgIUVfZslYEayBAiv6+V9lA5ktPf9jYwCgYIKoZIzj0EAwIw
>>"%WORKDIR%\%CRT%" echo GTEXMBUGA1UEAwwORGV2aWNlUHJveHkgQ0EwHhcNMjYwOTI5MDMwNTQzWhcNMzYw
>>"%WORKDIR%\%CRT%" echo OTI2MDMwNTQzWjAZMRcwFQYDVQQDDA5EZXZpY2VQcm94eSBDQTBZMBMGByqGSM49
>>"%WORKDIR%\%CRT%" echo AgEGCCqGSM49AwEHA0IABCh7WdaFBMOCXNjbRjaICJfAGQ2uCcBjKE+mDiqxCzxL
>>"%WORKDIR%\%CRT%" echo 9LbtiJm7iKZDg7FUvb6vPGdRSHhYUIwlAIDbgBTTQJmjYzBhMB0GA1UdDgQWBBTo
>>"%WORKDIR%\%CRT%" echo tHWQTTk1+kp3Gn/omx7RbgFd6TAfBgNVHSMEGDAWgBTotHWQTTk1+kp3Gn/omx7R
>>"%WORKDIR%\%CRT%" echo bgFd6TAPBgNVHRMBAf8EBTADAQH/MA4GA1UdDwEB/wQEAwIChDAKBggqhkjOPQQD
>>"%WORKDIR%\%CRT%" echo AgNJADBGAiEAsrLEFOvjAceXTc2WfnFCAoU0sGFVWrbOy3mj0A0r3CYCIQDYLtbX
>>"%WORKDIR%\%CRT%" echo It95OQr70jbFEwPw4mcqqHVaq59Vg35lGsF50Q==
>>"%WORKDIR%\%CRT%" echo -----END CERTIFICATE-----
:writeCAKey
if exist "%WORKDIR%\%KEY%" goto :writeCADone
>"%WORKDIR%\%KEY%" echo -----BEGIN PRIVATE KEY-----
>>"%WORKDIR%\%KEY%" echo MIGHAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBG0wawIBAQQgkFv91z9F6RVPe5cI
>>"%WORKDIR%\%KEY%" echo cKsnH6OQt66aQXn3bB+mQqJo+j2hRANCAAQoe1nWhQTDglzY20Y2iAiXwBkNrgnA
>>"%WORKDIR%\%KEY%" echo YyhPpg4qsQs8S/S27YiZu4imQ4OxVL2+rzxnUUh4WFCMJQCA24AU00CZ
>>"%WORKDIR%\%KEY%" echo -----END PRIVATE KEY-----
:writeCADone
echo CA pair written from embedded template.
exit /b

REM Request admin elevation for a subroutine - -Wait blocks until done; single quotes in path escaped; reports UAC cancel
:elevate
echo Administrator privileges required, requesting elevation...
set "ELEVATE_PATH=%~f0"
set ELEVATE_PATH=!ELEVATE_PATH:'=''!
powershell -NoProfile -Command "Start-Process -FilePath '!ELEVATE_PATH!' -Verb RunAs -Wait -ArgumentList '%~1'" >nul 2>&1
if not "!errorlevel!"=="0" echo Elevation cancelled or failed; operation not executed.
exit /b

REM Whether the current user is admin - net session success = admin
:isAdmin
set "ADMIN=0"
net session >nul 2>&1 && set "ADMIN=1"
exit /b

REM WinHTTP set - run directly by the elevated instance
:doProxySet
netsh winhttp set proxy 127.0.0.1:%PORT%
exit /b

REM WinHTTP reset - run directly by the elevated instance
:doProxyReset
netsh winhttp reset proxy
exit /b

:end
echo Goodbye.
endlocal
