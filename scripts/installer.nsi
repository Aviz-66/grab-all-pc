; GrabAll Download Manager — Windows Per-User Installer (NSIS Modern UI 2)
; Installs ONLY graball.exe + extension/ into %LOCALAPPDATA%\Programs\GrabAll
; (no UAC admin prompt required; folder stays writable for instant in-app auto-updates!)

Unicode True
SetCompressor /SOLID lzma

!include "MUI2.nsh"
!include "FileFunc.nsh"

!ifndef APP_VERSION
  !define APP_VERSION "2.5.4"
!endif
!ifndef SOURCE_DIR
  !define SOURCE_DIR "stage_win"
!endif
!ifndef OUT_FILE
  !define OUT_FILE "dist/GrabAll-Setup-windows-amd64.exe"
!endif
!ifndef ICON_FILE
  !define ICON_FILE "assets/icons/graball.ico"
!endif

!define APP_NAME "GrabAll Download Manager"
!define APP_EXE "graball.exe"
!define APP_PUBLISHER "GrabAll"
!define UNINSTALL_REG_KEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\GrabAll"
!define RUN_REG_KEY "Software\Microsoft\Windows\CurrentVersion\Run"

Name "${APP_NAME} ${APP_VERSION}"
OutFile "${OUT_FILE}"
InstallDir "$LOCALAPPDATA\Programs\GrabAll"
InstallDirRegKey HKCU "${UNINSTALL_REG_KEY}" "InstallLocation"
RequestExecutionLevel user
BrandingText "${APP_NAME} v${APP_VERSION}"

; Modern UI 2 settings & custom Android-matched GrabAll icon
!define MUI_ABORTWARNING
!define MUI_ICON "${ICON_FILE}"
!define MUI_UNICON "${ICON_FILE}"

!define MUI_WELCOMEPAGE_TITLE "Welcome to ${APP_NAME} Setup"
!define MUI_WELCOMEPAGE_TEXT "This wizard installs ${APP_NAME} v${APP_VERSION} and the bundled Chrome/Brave extension folder into your user profile (no Administrator prompt required).$\r$\n$\r$\nIn-app auto-updates and yt-dlp updates work seamlessly out of the box."

!define MUI_FINISHPAGE_RUN "$INSTDIR\${APP_EXE}"
!define MUI_FINISHPAGE_RUN_TEXT "Launch ${APP_NAME}"
!define MUI_FINISHPAGE_SHOWREADME "$INSTDIR\extension"
!define MUI_FINISHPAGE_SHOWREADME_NOTCHECKED
!define MUI_FINISHPAGE_SHOWREADME_TEXT "Open Browser Extension folder (to load in Chrome/Brave)"
!define MUI_FINISHPAGE_SHOWREADME_FUNCTION OpenExtensionFolder

!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_COMPONENTS
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH

!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES

!insertmacro MUI_LANGUAGE "English"

Function OpenExtensionFolder
  Exec 'explorer.exe "$INSTDIR\extension"'
FunctionEnd

Section "${APP_NAME} + Extension (Required)" SecCore
  SectionIn RO

  ; Gracefully close any running GrabAll instance before replacing graball.exe
  nsExec::ExecToLog 'taskkill.exe /F /IM ${APP_EXE}'
  Sleep 450

  SetOutPath "$INSTDIR"
  File "${SOURCE_DIR}/${APP_EXE}"

  ; Cleanly refresh bundled browser extension folder
  RMDir /r "$INSTDIR\extension"
  SetOutPath "$INSTDIR\extension"
  File /r "${SOURCE_DIR}/extension/*"

  ; Create Start Menu shortcuts
  SetOutPath "$INSTDIR"
  CreateDirectory "$SMPROGRAMS\GrabAll"
  CreateShortcut "$SMPROGRAMS\GrabAll\${APP_NAME}.lnk" "$INSTDIR\${APP_EXE}" "" "$INSTDIR\${APP_EXE}" 0
  CreateShortcut "$SMPROGRAMS\GrabAll\Browser Extension Folder.lnk" "$INSTDIR\extension"
  CreateShortcut "$SMPROGRAMS\GrabAll\Uninstall GrabAll.lnk" "$INSTDIR\Uninstall.exe"

  ; Write Uninstaller & Windows Installed Apps registry entry
  WriteUninstaller "$INSTDIR\Uninstall.exe"

  WriteRegStr HKCU "${UNINSTALL_REG_KEY}" "DisplayName" "${APP_NAME}"
  WriteRegStr HKCU "${UNINSTALL_REG_KEY}" "DisplayVersion" "${APP_VERSION}"
  WriteRegStr HKCU "${UNINSTALL_REG_KEY}" "Publisher" "${APP_PUBLISHER}"
  WriteRegStr HKCU "${UNINSTALL_REG_KEY}" "DisplayIcon" "$INSTDIR\${APP_EXE},0"
  WriteRegStr HKCU "${UNINSTALL_REG_KEY}" "InstallLocation" "$INSTDIR"
  WriteRegStr HKCU "${UNINSTALL_REG_KEY}" "UninstallString" '"$INSTDIR\Uninstall.exe"'
  WriteRegStr HKCU "${UNINSTALL_REG_KEY}" "QuietUninstallString" '"$INSTDIR\Uninstall.exe" /S'
  WriteRegStr HKCU "${UNINSTALL_REG_KEY}" "URLInfoAbout" "https://github.com/Aviz-66/grab-all-pc"
  WriteRegDWORD HKCU "${UNINSTALL_REG_KEY}" "NoModify" 1
  WriteRegDWORD HKCU "${UNINSTALL_REG_KEY}" "NoRepair" 1

  ${GetSize} "$INSTDIR" "/S=0K" $0 $1 $2
  IntFmt $0 "0x%08X" $0
  WriteRegDWORD HKCU "${UNINSTALL_REG_KEY}" "EstimatedSize" "$0"
SectionEnd

Section "Desktop Shortcut" SecDesktop
  SetOutPath "$INSTDIR"
  CreateShortcut "$DESKTOP\${APP_NAME}.lnk" "$INSTDIR\${APP_EXE}" "" "$INSTDIR\${APP_EXE}" 0
SectionEnd

Section "Start GrabAll with Windows (System Tray)" SecStartup
  WriteRegStr HKCU "${RUN_REG_KEY}" "GrabAll" '"$INSTDIR\${APP_EXE}" --tray'
  DeleteRegValue HKCU "Software\Microsoft\Windows\CurrentVersion\Explorer\StartupApproved\Run" "GrabAll"
SectionEnd

!insertmacro MUI_FUNCTION_DESCRIPTION_BEGIN
  !insertmacro MUI_DESCRIPTION_TEXT ${SecCore} "Installs graball.exe and the bundled Chrome/Brave extension folder."
  !insertmacro MUI_DESCRIPTION_TEXT ${SecDesktop} "Adds a GrabAll Download Manager shortcut to your Desktop."
  !insertmacro MUI_DESCRIPTION_TEXT ${SecStartup} "Automatically starts GrabAll quietly in the Windows system tray when you sign in."
!insertmacro MUI_FUNCTION_DESCRIPTION_END

Section "Uninstall"
  nsExec::ExecToLog 'taskkill.exe /F /IM ${APP_EXE}'
  Sleep 450

  Delete "$INSTDIR\${APP_EXE}"
  Delete "$INSTDIR\${APP_EXE}.old"
  Delete "$INSTDIR\${APP_EXE}.new"
  Delete "$INSTDIR\Uninstall.exe"
  RMDir /r "$INSTDIR\extension"
  RMDir /r "$INSTDIR\bin"
  RMDir /r "$INSTDIR\cache"
  RMDir /r "$INSTDIR\plugins"
  ; Remove install dir only if empty (preserves user config.json/queue.json if present, or removes cleanly)
  RMDir "$INSTDIR"

  Delete "$DESKTOP\${APP_NAME}.lnk"
  RMDir /r "$SMPROGRAMS\GrabAll"

  DeleteRegValue HKCU "${RUN_REG_KEY}" "GrabAll"
  DeleteRegValue HKCU "Software\Microsoft\Windows\CurrentVersion\Explorer\StartupApproved\Run" "GrabAll"
  DeleteRegKey HKCU "${UNINSTALL_REG_KEY}"
SectionEnd
