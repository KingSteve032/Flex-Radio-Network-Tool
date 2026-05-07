#define MyAppName "Flex Radio Network Tool"
#define MyAppPublisher "W4CAR"

#ifndef MyAppVersion
  #define MyAppVersion "dev"
#endif
#ifndef MySourceExe
  #error "MySourceExe define is required"
#endif
#ifndef MyIconPng
  #error "MyIconPng define is required"
#endif
#ifndef MySetupIcon
  #error "MySetupIcon define is required"
#endif
#ifndef MyOutputDir
  #define MyOutputDir "dist"
#endif

[Setup]
AppId={{D33831AB-78A6-4CA5-A9DB-31F96CF8CC6F}
AppName={#MyAppName}
AppVersion={#MyAppVersion}
AppPublisher={#MyAppPublisher}
DefaultDirName={autopf}\Flex Radio Network Tool
DefaultGroupName=Flex Radio Network Tool
AllowNoIcons=yes
DisableProgramGroupPage=no
DisableDirPage=no
DisableWelcomePage=no
OutputDir={#MyOutputDir}
OutputBaseFilename=frnt-windows-amd64-setup-{#MyAppVersion}
Compression=lzma
SolidCompression=yes
WizardStyle=modern
SetupIconFile={#MySetupIcon}
UninstallDisplayIcon={app}\icon.ico
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
PrivilegesRequired=admin

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Tasks]
Name: "startmenuicon"; Description: "Create a Start Menu shortcut"; GroupDescription: "Shortcuts:"; Flags: checkedonce
Name: "desktopicon"; Description: "Create a Desktop shortcut"; GroupDescription: "Shortcuts:"; Flags: unchecked

[Files]
Source: "{#MySourceExe}"; DestDir: "{app}"; DestName: "frnt.exe"; Flags: ignoreversion
Source: "{#MyIconPng}"; DestDir: "{app}"; DestName: "icon.png"; Flags: ignoreversion
Source: "{#MySetupIcon}"; DestDir: "{app}"; DestName: "icon.ico"; Flags: ignoreversion

[Icons]
Name: "{group}\Flex Radio Network Tool"; Filename: "{app}\frnt.exe"; WorkingDir: "{app}"; Tasks: startmenuicon; IconFilename: "{app}\icon.ico"
Name: "{autodesktop}\Flex Radio Network Tool"; Filename: "{app}\frnt.exe"; WorkingDir: "{app}"; Tasks: desktopicon; IconFilename: "{app}\icon.ico"

[Run]
Filename: "{app}\frnt.exe"; Description: "Launch Flex Radio Network Tool"; Flags: nowait postinstall skipifsilent
