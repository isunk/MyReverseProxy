@echo off
REM chcp 65001 若在本进程中途执行会让 cmd 解析器字节偏移错位（UTF-8 行被截断），
REM 故在父进程切码页后重新拉起自身，令整份脚本始终以 UTF-8 从头解析。
if defined MRP_UTF8 goto :mrpRun
chcp 65001 >nul
set "MRP_UTF8=1"
call "%~f0" %*
exit /b

:mrpRun
setlocal enabledelayedexpansion
title mrp 部署

REM ============================================================
REM  mrp 一键部署脚本
REM  两级菜单：先选部署目标（Windows / Android / HarmonyOS），
REM  再选一键部署或停止。
REM  CA 证书与私钥在首次部署时本地生成（不内置、不提交），
REM  每个用户独立持有，避免共享私钥破坏 MITM 信任模型。
REM ============================================================

:: 配置常量（改这里即可改默认值）
set "WORKDIR=%~dp0"
set "EXE=mrp-windows-amd64.exe"
set "CRT=ca.crt"
set "KEY=ca.key"
set "PORT=4000"
set "CN=DeviceProxy CA"
set "DEV_BIN=mrp-linux-arm64"
set "DEV_CFG=config.yaml"

:: 提权分发：提权实例带一个子程序名参数，执行后暂停展示结果再退出
if not "%~1"=="" (
    call :%~1
    echo.
    pause
    exit /b
)

:: ============================================================
:: 主循环：探测设备工具可用性后渲染主菜单
:: ============================================================
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

:: ============================================================
:: 主菜单：选择部署目标
:: ============================================================
:mainMenu
cls
echo ==============================
echo     mrp 部署
echo ==============================
echo [1] Windows
echo [2] Android       !ANDROID_DESC!
echo [3] HarmonyOS     !HARMONY_DESC!
echo ------------------------------
echo [0] 退出
set "QUIT="
set "CHOICE="
set /p "CHOICE=请选择 [0-3]: "
if "!CHOICE!"=="0" set "QUIT=1"
if "!CHOICE!"=="1" call :subWindows
if "!CHOICE!"=="2" call :subAndroid
if "!CHOICE!"=="3" call :subHarmony
exit /b

:: ============================================================
:: 设备工具探测：!TOOL! 是否安装且有已连接设备
::   输出 TOOL_READY（0/1）、TOOL_DESC（安装/连接描述）
:: ============================================================
:probeDevReady
set "TOOL_READY=0"
set "TOOL_DESC=未安装"
where !TOOL! >nul 2>&1
if not "!errorlevel!"=="0" exit /b
set "TOOL_DESC=已安装，未连接设备"
if /i "!TOOL!"=="adb" (
    for /f "skip=1 tokens=2" %%i in ('adb devices 2^>nul') do if "!TOOL_READY!"=="0" if "%%i"=="device" set "TOOL_READY=1"
) else (
    for /f "tokens=1" %%i in ('hdc list targets 2^>nul') do if "!TOOL_READY!"=="0" if not "%%i"=="[Empty]" if not "%%i"=="Connect" set "TOOL_READY=1"
)
if "!TOOL_READY!"=="1" set "TOOL_DESC=已连接设备"
exit /b

:: ============================================================
:: Windows 子菜单
:: ============================================================
:subWindows
:winLoop
call :probeWin
cls
echo ==============================
echo     Windows 部署
echo ==============================
echo [A] 文件就位      !A_DESC!
echo [B] CA 证书       !B_DESC!
echo [C] mrp 进程      !C_DESC!
echo [D] 系统代理      !D_DESC!
echo ------------------------------
echo [1] 一键部署
echo [2] 停止（关进程 + 清代理）
echo [0] 返回
set "CHOICE="
set /p "CHOICE=请选择: "
if "!CHOICE!"=="1" (call :winDeploy & echo. & pause & goto :winLoop)
if "!CHOICE!"=="2" (call :winStop & echo. & pause & goto :winLoop)
if "!CHOICE!"=="0" exit /b
goto :winLoop

:: Windows 状态探测与描述
:probeWin
call :checkFiles
call :probeCert
call :probeRun
call :probeProxy
if "!A_STATE!"=="OK" (set "A_DESC=OK") else (set "A_DESC=缺失!MISS!")
if "!B_STATE!"=="NONE" (set "B_DESC=未导入") else (set "B_DESC=已导入 [!B_STATE!]")
if "!C_STATE!"=="RUN" (set "C_DESC=运行中") else (set "C_DESC=未运行")
if "!D_STATE!"=="ON" (set "D_DESC=已设置") else (set "D_DESC=未设置")
exit /b

:: ============================================================
:: Windows 一键部署：准备文件 → 导入 CA → 启动 mrp → 设置代理
::   每步先探测，已完成则跳过
:: ============================================================
:winDeploy
echo.
echo [1/4] 准备文件
call :ensureCA
if "!EXE_MISSING!"=="1" call :fetchFile %EXE%
call :checkFiles
if "!A_STATE!"=="BAD" (echo 文件仍缺失，无法继续部署。 & exit /b 1)

echo [2/4] 导入 CA 证书
call :probeCert
if "!B_STATE!"=="NONE" (certutil -user -addstore Root "%WORKDIR%\%CRT%") else (echo 已导入（!B_STATE!），跳过。)

echo [3/4] 启动 mrp
call :probeRun
if "!C_STATE!"=="RUN" (echo mrp 已在运行，跳过。) else (start "mrp" /D "%WORKDIR%" "%WORKDIR%\%EXE%" & echo mrp 已在新窗口启动。)

echo [4/4] 设置系统代理
call :probeProxy
if "!D_STATE!"=="ON" (
    echo 系统代理已设置，跳过。
) else (
    call :runElevated doProxySet
    call :probeProxy
    if "!D_STATE!"=="ON" (echo 系统代理已设置为 127.0.0.1:!PORT!。) else (echo 警告：系统代理设置未生效，可能取消了授权或命令失败。)
)
echo Windows 部署完成。
exit /b

:: Windows 停止：关闭进程并清空系统代理
:winStop
echo.
echo 停止 mrp 进程...
call :probeRun
if "!C_STATE!"=="RUN" (taskkill /im "%EXE%" /f) else (echo mrp 未运行，跳过。)
echo 清空系统代理...
call :runElevated doProxyReset
call :probeProxy
if "!D_STATE!"=="ON" echo 警告：系统代理清除未生效。
echo 已停止。
exit /b

:: ============================================================
:: 设备子菜单：Android / HarmonyOS 共用，DEV_TOOL 区分
:: ============================================================
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
set "DEV_FILE_DESC=未就位"
set "DEV_RUN_DESC=未运行"
set "DEV_PROXY_DESC=未知"
if "!TOOL_READY!"=="1" call :probeDevState
cls
echo ==============================
echo     !DEV_NAME! 部署
echo ==============================
echo 设备工具：!DEV_TOOL!（!TOOL_DESC!）
echo ------------------------------
if "!TOOL_READY!"=="1" (
    echo [A] 设备文件      !DEV_FILE_DESC!
    echo [B] 设备进程      !DEV_RUN_DESC!
    echo [C] 设备代理      !DEV_PROXY_DESC!
) else (
    echo 设备未连接，请检查 USB 调试与连接状态。
)
echo ------------------------------
echo [1] 一键部署
echo [2] 停止（关进程 + 清代理）
echo [0] 返回
set "CHOICE="
set /p "CHOICE=请选择: "
if "!CHOICE!"=="0" exit /b
if "!TOOL_READY!"=="0" (echo 未连接设备，无法执行。 & timeout /t 2 >nul & goto :devLoop)
if "!CHOICE!"=="1" (call :devDeploy & echo. & pause & goto :devLoop)
if "!CHOICE!"=="2" (call :devStop & echo. & pause & goto :devLoop)
goto :devLoop

:: 设备路径与推送命令：adb 与 hdc 的临时目录、系统证书目录、推送动词不同
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

:: 设备状态懒探测：文件 / 进程 / 代理
:probeDevState
!DEV_TOOL! shell test -f !DEV_REMOTE!/!DEV_BIN! >nul 2>&1
if "!errorlevel!"=="0" set "DEV_FILE_DESC=已就位"
!DEV_TOOL! shell pidof !DEV_BIN! >nul 2>&1
if "!errorlevel!"=="0" set "DEV_RUN_DESC=运行中"
if /i not "!DEV_TOOL!"=="adb" exit /b
set "PROXY_VALUE="
for /f "delims=" %%i in ('!DEV_TOOL! shell settings get global http_proxy 2^>nul') do set "PROXY_VALUE=%%i"
if "!PROXY_VALUE!"=="127.0.0.1:!PORT!" set "DEV_PROXY_DESC=已设置"
exit /b

:: ============================================================
:: 设备一键部署：准备文件 → 推送 → 安装 CA → 启动 → 设置代理
:: ============================================================
:devDeploy
echo.
echo [1/5] 准备文件
call :ensureCA
call :checkDevFiles
if "!DEV_BIN_MISSING!"=="1" call :fetchFile %DEV_BIN%
call :checkDevFiles
if "!DEV_BIN_MISSING!"=="1" (echo 缺少 %DEV_BIN%，无法继续部署。 & exit /b 1)
if "!CRT_MISSING!"=="1" (echo 缺少 CA 证书，无法继续部署。 & exit /b 1)
if "!KEY_MISSING!"=="1" (echo 缺少 CA 私钥，无法继续部署。 & exit /b 1)

echo [2/5] 推送文件到设备
call :devPush

echo [3/5] 安装 CA 到设备系统证书
call :devCert

echo [4/5] 在设备上启动 mrp
call :devRun

echo [5/5] 设置设备全局代理
call :devProxyCfg on
echo 设备部署完成。
exit /b

:: 设备停止：结束 mrp 进程并清空全局代理
:devStop
echo.
echo 停止设备 mrp 进程...
!DEV_TOOL! shell pkill -f !DEV_BIN! >nul 2>&1
echo 清空设备全局代理...
call :devProxyCfg off
echo 已停止。
exit /b

:: ============================================================
:: 文件与状态探测子程序
:: ============================================================

:: 确保本地 CA 与二进制就位：缺 CA 则生成，缺二进制则提示复制
:ensureCA
call :checkFiles
set "NEED_CA=0"
if "!CRT_MISSING!"=="1" set "NEED_CA=1"
if "!KEY_MISSING!"=="1" set "NEED_CA=1"
if "!NEED_CA!"=="1" call :genCA
call :checkFiles
exit /b

:: 缺二进制时向用户索取所在目录并复制进来（%1 = 文件名）
:fetchFile
echo 未找到 %~1。请输入包含它的目录（GitHub Releases 下载后所在目录）：
set "SRC="
set /p "SRC=目录: "
if not defined SRC exit /b
set SRC=!SRC:"=!
if exist "!SRC!\%~1" (copy /y "!SRC!\%~1" "%WORKDIR%\" >nul & echo 已复制 %~1。) else (echo 该目录下未找到 %~1。)
exit /b

:: EXE / ca.crt / ca.key 是否就位
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

:: CA 是否已导入用户/机器 Root 存储
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

:: mrp 进程是否运行
:probeRun
set "C_STATE=STOP"
tasklist /fi "imagename eq %EXE%" /nh 2>nul | find /i "%EXE%" >nul && set "C_STATE=RUN"
exit /b

:: WinHTTP 系统代理是否指向本机端口
:probeProxy
set "D_STATE=OFF"
netsh winhttp show proxy 2>nul | find /i "127.0.0.1:%PORT%" >nul && set "D_STATE=ON"
exit /b

:: 本地设备二进制是否就位
:checkDevFiles
set "DEV_BIN_MISSING=0"
if not exist "%WORKDIR%\%DEV_BIN%" set "DEV_BIN_MISSING=1"
exit /b

:: ============================================================
:: 设备操作子程序
:: ============================================================

:: 推送二进制、配置与 CA 到设备（PUSHCMD 区分 push / file send）
:devPush
echo 推送文件到设备（!DEV_TOOL!）...
if /i not "!DEV_TOOL!"=="adb" !DEV_TOOL! shell mkdir -p !DEV_REMOTE!
!DEV_TOOL! !PUSHCMD! "%WORKDIR%\%DEV_BIN%" !DEV_REMOTE!
if exist "%WORKDIR%\%DEV_CFG%" !DEV_TOOL! !PUSHCMD! "%WORKDIR%\%DEV_CFG%" !DEV_REMOTE!
!DEV_TOOL! !PUSHCMD! "%WORKDIR%\%CRT%" !DEV_REMOTE!
!DEV_TOOL! !PUSHCMD! "%WORKDIR%\%KEY%" !DEV_REMOTE!
!DEV_TOOL! shell chmod +x !DEV_REMOTE!/!DEV_BIN!
echo 文件已推送。
exit /b

:: 安装 CA 到设备系统证书目录（按 subject_hash_old 命名）
:devCert
echo 安装 CA 证书到设备系统证书目录...
call :findOpenSSL
if "!OSSL!"=="" (echo 未检测到 openssl，无法计算证书哈希，跳过设备证书安装。 & exit /b)
set "HASH="
for /f "delims=" %%i in ('"!OSSL!" x509 -subject_hash_old -in "%WORKDIR%\%CRT%" 2^>nul') do if not defined HASH set "HASH=%%i"
if not defined HASH (echo 无法计算 CA 证书哈希，跳过设备证书安装。 & exit /b)
echo 证书哈希：!HASH!.0
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
if "!errorlevel!"=="0" (echo CA 已安装为 !DEV_CERTS!/!HASH!.0。) else (echo 警告：证书安装可能失败，请确认设备已 Root 或处于开发者模式。)
exit /b

:: 在设备上启动 mrp（新窗口保留日志，已在运行则跳过）
:devRun
!DEV_TOOL! shell pidof !DEV_BIN! >nul 2>&1
if "!errorlevel!"=="0" (
    echo mrp 已在设备上运行，跳过启动。
    exit /b
)
echo 在设备上启动 mrp...
start "mrp 设备" cmd /k !DEV_TOOL! shell "cd !DEV_REMOTE!; ./!DEV_BIN!"
echo mrp 已在新窗口启动（Ctrl+C 停止，关闭窗口退出）。
exit /b

:: 设备全局 HTTP 代理：on 设置端口代理，off 清空（adb/hdc 清空值不同）
:devProxyCfg
set "V_ADB=127.0.0.1:!PORT!"
set "V_HDC=127.0.0.1:!PORT!"
if /i "%~1"=="off" (set "V_ADB=:0" & set "V_HDC=0")
if /i "!DEV_TOOL!"=="adb" (
    !DEV_TOOL! shell settings put global http_proxy !V_ADB!
) else (
    !DEV_TOOL! shell network-cfg set http_proxy !V_HDC!
)
if not "!errorlevel!"=="0" (echo 警告：设备代理设置失败，请检查设备连接。 & exit /b)
if /i "%~1"=="off" (echo 设备全局代理已清空。) else (echo 设备全局代理已设置为 127.0.0.1:!PORT!。)
exit /b

:: ============================================================
:: 通用工具子程序
:: ============================================================

:: 需管理员的操作：当前已是管理员则直接执行，否则提权执行（%1 = 子程序名）
:runElevated
call :isAdmin
if "!ADMIN!"=="1" (call :%~1) else (call :elevate %~1)
exit /b

:: 本地生成自签 CA（仅当 ca.crt / ca.key 缺失）
:genCA
echo 缺少 CA 证书/私钥，开始本地生成...
call :findOpenSSL
if "!OSSL!"=="" (echo 未检测到 openssl。请安装 Git for Windows，或按 README 用 openssl 手动生成 ca.crt / ca.key 后重试。 & exit /b 1)
"!OSSL!" req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -keyout "%WORKDIR%\%KEY%" -out "%WORKDIR%\%CRT%" -nodes -days 3650 -subj "/CN=%CN%" -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,digitalSignature" >nul 2>&1
if exist "%WORKDIR%\%CRT%" (echo 已生成 %CRT% 与 %KEY%。) else (echo 生成失败，请检查 openssl 是否可用。)
exit /b

:: 探测 openssl 路径（PATH 优先，再查 Git for Windows 常见位置）
:findOpenSSL
set "OSSL="
where openssl >nul 2>&1 && set "OSSL=openssl"
if "!OSSL!"=="" if exist "C:\Program Files\Git\usr\bin\openssl.exe" set "OSSL=C:\Program Files\Git\usr\bin\openssl.exe"
if "!OSSL!"=="" if exist "C:\Program Files\Git\mingw64\bin\openssl.exe" set "OSSL=C:\Program Files\Git\mingw64\bin\openssl.exe"
if "!OSSL!"=="" if exist "C:\Program Files (x86)\Git\usr\bin\openssl.exe" set "OSSL=C:\Program Files (x86)\Git\usr\bin\openssl.exe"
exit /b

:: 请求管理员提权执行指定子程序（-Wait 阻塞至完成；路径中单引号转义，取消 UAC 时如实报告）
:elevate
echo 需要管理员权限，正在请求提权...
set "ELEVATE_PATH=%~f0"
set ELEVATE_PATH=!ELEVATE_PATH:'=''!
powershell -NoProfile -Command "Start-Process -FilePath '!ELEVATE_PATH!' -Verb RunAs -Wait -ArgumentList '%~1'" >nul 2>&1
if not "!errorlevel!"=="0" echo 提权被取消或失败，操作未执行。
exit /b

:: 当前是否管理员（net session 成功即管理员）
:isAdmin
set "ADMIN=0"
net session >nul 2>&1 && set "ADMIN=1"
exit /b

:: WinHTTP 设置（提权实例直接执行）
:doProxySet
netsh winhttp set proxy 127.0.0.1:%PORT%
exit /b

:: WinHTTP 重置（提权实例直接执行）
:doProxyReset
netsh winhttp reset proxy
exit /b

:end
echo 再见。
endlocal
