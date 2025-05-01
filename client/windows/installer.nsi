; Plurality Installer Script

;--------------------------------
; Includes
;--------------------------------
!include MUI2.nsh ; Modern UI 2

;--------------------------------
; Defines (Constants) - Customize these!
;--------------------------------
!define APPNAME "Plurality"
!define COMPANYNAME "Beyond Cloud ltd" ; <-- !!! Change this to your company/developer name
!define DESCRIPTION "Plurality Application"
!define VERSION "1.0.0" ; <-- !!! Update this with your actual app version (maybe dynamically later)
!define INSTALLER_OUTPUT_FILENAME "plurality-installer.exe" ; Output name (matches workflow 'cp' source)
!define MAIN_EXECUTABLE "Plurality.exe" ; <-- !!! Verify this is the name of your built Flutter executable
!define BUILD_OUTPUT_PATH "..\build\windows\x64\runner\Release" ; Relative path from the script location to the built app
!define INSTALL_DIR_REGKEY "Software\${COMPANYNAME}\${APPNAME}"

;--------------------------------
; General Installer Attributes
;--------------------------------
Name "${APPNAME} ${VERSION}"
OutFile "${INSTALLER_OUTPUT_FILENAME}"
InstallDir "$PROGRAMFILES64\${APPNAME}" ; Default install directory (64-bit)
InstallDirRegKey HKLM "${INSTALL_DIR_REGKEY}" ""
RequestExecutionLevel admin ; Request admin privileges to write to Program Files

; Set compression
SetCompressor lzma

;--------------------------------
; Version Information (for installer exe properties)
;--------------------------------
VIProductVersion "${VERSION}.0"
VIAddVersionKey "ProductName" "${APPNAME}"
VIAddVersionKey "CompanyName" "${COMPANYNAME}"
VIAddVersionKey "FileDescription" "${DESCRIPTION}"
VIAddVersionKey "FileVersion" "${VERSION}"
VIAddVersionKey "LegalCopyright" "Copyright © ${COMPANYNAME}" ; <-- Adjust if needed

;--------------------------------
; Modern UI Interface Settings
;--------------------------------
!define MUI_ABORTWARNING ; Warn if the user tries to cancel during installation
!define MUI_ICON "./runner/resources/app_icon.ico" ; <-- !!! Optional: Path to your app icon (.ico file) relative to script
!define MUI_UNICON "./runner/resources/app_icon.ico" ; <-- !!! Optional: Path to your uninstall icon (.ico file) relative to script

;--------------------------------
; Pages
;--------------------------------
!insertmacro MUI_PAGE_WELCOME
; !insertmacro MUI_PAGE_LICENSE "path\to\your\license.txt" ; <-- !!! Optional: Uncomment and provide path to license file
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES ; Installation progress page
!insertmacro MUI_PAGE_FINISH

; Uninstaller Pages
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES

;--------------------------------
; Languages
;--------------------------------
!insertmacro MUI_LANGUAGE "English"

;--------------------------------
; Installation Section
;--------------------------------
Section "Install" SEC_INSTALL
  SetOutPath $INSTDIR

  ; We expect this script to be in client/installer, so go up one and into build/...
  SetOverwrite ifnewer ; Don't overwrite newer files (optional)
  File /r "${BUILD_OUTPUT_PATH}\*.*" ; Copy all files from Flutter build output recursively

  ; --- Create Uninstaller ---
  WriteUninstaller "$INSTDIR\uninstall.exe"

  ; --- Create Start Menu Shortcuts ---
  CreateDirectory "$SMPROGRAMS\${APPNAME}"
  CreateShortCut "$SMPROGRAMS\${APPNAME}\${APPNAME}.lnk" "$INSTDIR\${MAIN_EXECUTABLE}"
  CreateShortCut "$SMPROGRAMS\${APPNAME}\Uninstall ${APPNAME}.lnk" "$INSTDIR\uninstall.exe"

  ; --- Write Registry Keys for Add/Remove Programs ---
  WriteRegStr HKLM "${INSTALL_DIR_REGKEY}" "Install_Dir" "$INSTDIR"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${APPNAME}" "DisplayName" "${APPNAME}"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${APPNAME}" "UninstallString" '"$INSTDIR\uninstall.exe"'
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${APPNAME}" "QuietUninstallString" '"$INSTDIR\uninstall.exe" /S' ; For silent uninstall
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${APPNAME}" "DisplayIcon" "$INSTDIR\${MAIN_EXECUTABLE}"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${APPNAME}" "Publisher" "${COMPANYNAME}"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${APPNAME}" "DisplayVersion" "${VERSION}"
  WriteRegDWORD HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${APPNAME}" "NoModify" 1
  WriteRegDWORD HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${APPNAME}" "NoRepair" 1
  ; Estimate size (optional, requires calculation or setting manually)
  ; WriteRegDWORD HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${APPNAME}" "EstimatedSize" size_in_kb

SectionEnd

;--------------------------------
; Uninstallation Section
;--------------------------------
Section "Uninstall" SEC_UNINSTALL
  ; --- Remove Files and Installation Directory ---
  Delete "$INSTDIR\uninstall.exe" ; Delete the uninstaller itself
  RMDir /r "$INSTDIR" ; Remove the installation directory and all its contents

  ; --- Remove Start Menu Shortcuts ---
  Delete "$SMPROGRAMS\${APPNAME}\*.lnk"
  RMDir "$SMPROGRAMS\${APPNAME}"

  ; --- Remove Registry Keys ---
  DeleteRegKey HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${APPNAME}"
  DeleteRegKey HKLM "${INSTALL_DIR_REGKEY}"

SectionEnd

;--------------------------------
; Functions (for MUI) - Usually no changes needed here
;--------------------------------
Function .onInit
  !insertmacro MUI_LANGDLL_DISPLAY
FunctionEnd
