#include "flutter_window.h"

#include <cstdarg>
#include <cstdio>
#include <cstring>
#include <optional>
#include <windows.h>

// Registry (advapi32) is used to persist the window size across launches.
#pragma comment(lib, "advapi32.lib")

#include "flutter/generated_plugin_registrant.h"

FlutterWindow::FlutterWindow(const flutter::DartProject& project)
    : project_(project) {}

FlutterWindow::~FlutterWindow() {}

// Writes one line to %TEMP%\plurality_fancyzones.log. Used to diagnose the
// FancyZones blank-window bug on real machines; cheap enough to leave in.
void FlutterWindow::LogFz(const char* fmt, ...) {
  char path[MAX_PATH];
  if (GetTempPathA(MAX_PATH, path) == 0) {
    return;
  }
  std::snprintf(path + strlen(path), MAX_PATH - strlen(path),
                "plurality_fancyzones.log");
  FILE* f = nullptr;
  if (fopen_s(&f, path, "a") != 0 || f == nullptr) {
    return;
  }
  char line[1024];
  va_list args;
  va_start(args, fmt);
  std::vsnprintf(line, sizeof(line), fmt, args);
  va_end(args);
  fprintf(f, "[%llu] %s\n",
          static_cast<unsigned long long>(GetTickCount64()), line);
  fclose(f);
}

void FlutterWindow::SaveSizeToRegistry() {
  HWND parent = GetHandle();
  if (!parent) {
    return;
  }
  RECT r{};
  if (!GetWindowRect(parent, &r)) {
    return;
  }
  HKEY key = nullptr;
  if (RegCreateKeyExA(HKEY_CURRENT_USER, "Software\\AseraCorp\\Plurality",
                      0, nullptr, 0, KEY_SET_VALUE, nullptr, &key, nullptr) ==
      ERROR_SUCCESS) {
    DWORD v;
    v = (DWORD)r.left;  RegSetValueExA(key, "Left", 0, REG_DWORD, (BYTE*)&v, 4);
    v = (DWORD)r.top;   RegSetValueExA(key, "Top", 0, REG_DWORD, (BYTE*)&v, 4);
    v = (DWORD)(r.right - r.left);  RegSetValueExA(key, "W", 0, REG_DWORD, (BYTE*)&v, 4);
    v = (DWORD)(r.bottom - r.top);  RegSetValueExA(key, "H", 0, REG_DWORD, (BYTE*)&v, 4);
    LogFz("SaveSizeToRegistry: %d,%d %dx%d", r.left, r.top,
          r.right - r.left, r.bottom - r.top);
    RegCloseKey(key);
  }
}

void FlutterWindow::LoadSavedSize() {
  HKEY key = nullptr;
  if (RegOpenKeyExA(HKEY_CURRENT_USER, "Software\\AseraCorp\\Plurality", 0,
                    KEY_QUERY_VALUE, &key) != ERROR_SUCCESS) {
    LogFz("LoadSavedSize: no registry entry");
    return;
  }
  DWORD w = 0, h = 0;
  LONG x = 0, y = 0;
  DWORD type = REG_DWORD, size = 4;
  BOOL ok = RegQueryValueExA(key, "W", nullptr, &type, (BYTE*)&w, &size) == ERROR_SUCCESS &&
            RegQueryValueExA(key, "H", nullptr, &type, (BYTE*)&h, &size) == ERROR_SUCCESS &&
            RegQueryValueExA(key, "Left", nullptr, &type, (BYTE*)&x, &size) == ERROR_SUCCESS &&
            RegQueryValueExA(key, "Top", nullptr, &type, (BYTE*)&y, &size) == ERROR_SUCCESS;
  RegCloseKey(key);
  if (!ok || w < 200u || h < 200u) {
    LogFz("LoadSavedSize: invalid saved size (%lu x %lu)", w, h);
    return;
  }
  // Sanity: keep the window on the virtual screen (visible). If the saved
  // position is off-screen, let Windows place it.
  RECT work{};
  if (SystemParametersInfoA(SPI_GETWORKAREA, 0, &work, 0)) {
    if (x < work.left - 100 || y < work.top - 100) {
      x = work.left + 40;
      y = work.top + 40;
    }
  }
  LogFz("LoadSavedSize: restoring %d,%d %lux%lu", x, y, w, h);
  SetWindowPos(GetHandle(), nullptr, (int)x, (int)y, (int)w, (int)h,
               SWP_NOZORDER | SWP_NOACTIVATE);
}

bool FlutterWindow::OnCreate() {
  if (!Win32Window::OnCreate()) {
    return false;
  }
  LogFz("OnCreate");
  // Always start at the size/position the app was closed with — ignore what
  // FancyZones thinks the "last zone" was.
  LoadSavedSize();
  Show();

  // Create the Flutter view immediately (no deferred-startup waits): the
  // startup lockdown below prevents FancyZones from moving/resizing the
  // window during the first frames, so the view is created once at the
  // correct, final size and renders cleanly.
  if (!CreateFlutterView()) {
    LogFz("CreateFlutterView FAILED");
    return false;
  }

  // Startup lockdown: neutralize external geometry changes (FancyZones "move
  // new windows to last zone") until the view is up and the window has shown
  // its own frames. After kStartupLockMs the window is free to snap/drag.
  startup_locked_ = true;
  SetTimer(GetHandle(), kStartupUnlockTimer, (UINT)kStartupLockMs, nullptr);
  LogFz("startup lockdown ON for %u ms", (UINT)kStartupLockMs);
  return true;
}

bool FlutterWindow::CreateFlutterView() {
  if (flutter_controller_) {
    return true;
  }

  RECT frame = GetClientArea();
  int w = frame.right - frame.left;
  int h = frame.bottom - frame.top;
  LogFz("CreateFlutterView at %dx%d", w, h);

  flutter_controller_ = std::make_unique<flutter::FlutterViewController>(
      w, h, project_);
  if (!flutter_controller_->engine() || !flutter_controller_->view()) {
    flutter_controller_ = nullptr;
    return false;
  }

  RegisterPlugins(flutter_controller_->engine());
  SetChildContent(flutter_controller_->view()->GetNativeWindow());

  flutter_controller_->engine()->SetNextFrameCallback([&]() {
    this->Show();
  });

  // Ensure a frame is pending so the window is shown as soon as possible.
  flutter_controller_->ForceRedraw();
  return true;
}

void FlutterWindow::OnDestroy() {
  KillTimer(GetHandle(), kStartupUnlockTimer);
  SaveSizeToRegistry();
  if (flutter_controller_) {
    flutter_controller_ = nullptr;
  }
  LogFz("OnDestroy");
  Win32Window::OnDestroy();
}

LRESULT
FlutterWindow::MessageHandler(HWND hwnd, UINT const message,
                              WPARAM const wparam,
                              LPARAM const lparam) noexcept {
  if (flutter_controller_) {
    std::optional<LRESULT> result =
        flutter_controller_->HandleTopLevelWindowProc(hwnd, message, wparam,
                                                      lparam);
    if (result) {
      return *result;
    }
  }

  switch (message) {
    case WM_FONTCHANGE:
      flutter_controller_->engine()->ReloadSystemFonts();
      break;

    case WM_WINDOWPOSCHANGING: {
      // While in startup lockdown, neutralize geometry changes initiated by
      // other processes (PowerToys FancyZones "move to last zone"). Setting
      // SWP_NOMOVE|SWP_NOSIZE in the WINDOWPOS cancels the move/resize the
      // window manager is about to apply.
      if (startup_locked_) {
        auto* pos = reinterpret_cast<WINDOWPOS*>(lparam);
        if (pos) {
          pos->flags |= SWP_NOMOVE | SWP_NOSIZE;
          LogFz("WM_WINDOWPOSCHANGING LOCKED %dx%d (lockdown)",
                pos->cx, pos->cy);
        }
      }
      break;
    }

    case WM_TIMER:
      if (wparam == kStartupUnlockTimer) {
        KillTimer(hwnd, kStartupUnlockTimer);
        startup_locked_ = false;
        LogFz("startup lockdown OFF (window free to snap/drag)");
        return 0;
      }
      break;
  }

  return Win32Window::MessageHandler(hwnd, message, wparam, lparam);
}
