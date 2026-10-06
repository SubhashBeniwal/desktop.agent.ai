; AIO Agent Windows installer (NSIS 3).
;
; Built by packaging/windows/build-installer.sh, which passes:
;   /DVERSION=1.2.3  /DEXE=<path to aio-agent.exe>  /DICON=<path to .ico>  /DOUTFILE=<setup.exe>
;
; Installs per-user (no admin prompt) into %LocalAppData%\Programs\AIO Agent.

Unicode true
!include "MUI2.nsh"

!define APPNAME   "AIO Agent"
!define EXENAME   "AIO Agent.exe"
!define UNINSTKEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\AIOAgent"
; Value name Wails uses for "Launch at login" (slug of the app name).
!define RUNVALUE  "aio-agent"

Name "${APPNAME}"
OutFile "${OUTFILE}"
InstallDir "$LOCALAPPDATA\Programs\${APPNAME}"
InstallDirRegKey HKCU "${UNINSTKEY}" "InstallLocation"
RequestExecutionLevel user
SetCompressor /SOLID lzma

VIProductVersion "${VERSION}.0"
VIAddVersionKey "ProductName" "${APPNAME}"
VIAddVersionKey "FileDescription" "${APPNAME} Installer"
VIAddVersionKey "ProductVersion" "${VERSION}"
VIAddVersionKey "FileVersion" "${VERSION}"
VIAddVersionKey "LegalCopyright" "© AIO Agent"

!define MUI_ICON "${ICON}"
!define MUI_UNICON "${ICON}"
!define MUI_ABORTWARNING
!define MUI_FINISHPAGE_RUN "$INSTDIR\${EXENAME}"
!define MUI_FINISHPAGE_RUN_TEXT "Launch ${APPNAME}"

!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "English"

; Stop a running copy so its exe can be replaced/removed.
!macro StopApp
  nsExec::Exec 'taskkill /F /IM "${EXENAME}"'
  Pop $0
  Sleep 500
!macroend

Section "Install"
  !insertmacro StopApp
  SetOutPath "$INSTDIR"
  File "/oname=${EXENAME}" "${EXE}"
  WriteUninstaller "$INSTDIR\Uninstall.exe"

  CreateShortcut "$SMPROGRAMS\${APPNAME}.lnk" "$INSTDIR\${EXENAME}"
  CreateShortcut "$DESKTOP\${APPNAME}.lnk" "$INSTDIR\${EXENAME}"

  WriteRegStr   HKCU "${UNINSTKEY}" "DisplayName" "${APPNAME}"
  WriteRegStr   HKCU "${UNINSTKEY}" "DisplayVersion" "${VERSION}"
  WriteRegStr   HKCU "${UNINSTKEY}" "Publisher" "AIO Agent"
  WriteRegStr   HKCU "${UNINSTKEY}" "DisplayIcon" "$INSTDIR\${EXENAME}"
  WriteRegStr   HKCU "${UNINSTKEY}" "InstallLocation" "$INSTDIR"
  WriteRegStr   HKCU "${UNINSTKEY}" "UninstallString" '"$INSTDIR\Uninstall.exe"'
  WriteRegDWORD HKCU "${UNINSTKEY}" "NoModify" 1
  WriteRegDWORD HKCU "${UNINSTKEY}" "NoRepair" 1
SectionEnd

Section "Uninstall"
  !insertmacro StopApp
  Delete "$INSTDIR\${EXENAME}"
  Delete "$INSTDIR\Uninstall.exe"
  RMDir "$INSTDIR"
  Delete "$SMPROGRAMS\${APPNAME}.lnk"
  Delete "$DESKTOP\${APPNAME}.lnk"
  DeleteRegValue HKCU "Software\Microsoft\Windows\CurrentVersion\Run" "${RUNVALUE}"
  DeleteRegKey HKCU "${UNINSTKEY}"
  ; Settings in %AppData%\AIO Agent are kept so a reinstall reconnects.
SectionEnd
