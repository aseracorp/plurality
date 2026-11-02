#include "flutter_window.h"

#include <cstdarg>
#include <cstdio>
#include <cstring>
#include <optional>

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

void FlutterWindow::KickRedraw() {
  if (!flutter_controller_) {
    return;
  }

  HWND parent = GetHandle();
  RECT rect = GetClientArea();
  int w = rect.right - rect.left;
  int h = rect.bottom - rect.top;
  LogFz("KickRedraw: client %dx%d", w, h);

  if (w <= 0 || h <= 0 || kicking_) {
    return;
  }
  kicking_ = true;

  // The only action that reliably un-blanks the window on FancyZones setups
  // is a real parent resize (what the user does by unsnapping). A ForceRedraw
  // or a child-only resize does not repropagate the viewport metrics through
  // the engine. Shrink the parent by 1px and restore it — this triggers real
  // WM_SIZE / WM_WINDOWPOSCHANGED on the top-level window at the final size,
  // exactly like a manual un-snap, and the engine re-presents for a fresh
  // frame. The WM_SIZE handler ignores the resulting messages while
  // kicking_ is set, so no kick loop.
  RECT window_rect;
  GetWindowRect(parent, &window_rect);
  LogFz("  nudge: %d,%d %dx%d -> %dx%d", window_rect.left, window_rect.top,
        window_rect.right - window_rect.left, window_rect.bottom - window_rect.top,
        (window_rect.right - window_rect.left) - 1,
        (window_rect.bottom - window_rect.top) - 1);
  SetWindowPos(parent, nullptr, window_rect.left, window_rect.top,
               (window_rect.right - window_rect.left) - 1,
               (window_rect.bottom - window_rect.top) - 1,
               SWP_NOZORDER | SWP_NOACTIVATE);
  SetWindowPos(parent, nullptr, window_rect.left, window_rect.top,
               window_rect.right - window_rect.left,
               window_rect.bottom - window_rect.top,
               SWP_NOZORDER | SWP_NOACTIVATE);

  // Also force a redraw of the controller, matching the manual state after a
  // successful unsnap.
  if (flutter_controller_) {
    flutter_controller_->ForceRedraw();
  }

  kicking_ = false;
  LogFz("  KickRedraw done");
}

bool FlutterWindow::CreateFlutterView() {
  if (flutter_started_) {
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
  flutter_started_ = true;

  RegisterPlugins(flutter_controller_->engine());
  SetChildContent(flutter_controller_->view()->GetNativeWindow());

  flutter_controller_->engine()->SetNextFrameCallback([&]() {
    this->Show();
  });

  flutter_controller_->ForceRedraw();

  // Deterministic, staggered repaint kicks. FancyZones can move/resize the
  // window at any point during startup; a single kick often misses the real
  // final size, so fire several over the first ~1.5s. Each uses a DISTINCT
  // timer id (kRepaintKickTimerBase + i) — SetTimer with the same id would
  // just reset that one timer instead of creating a second.
  for (unsigned i = 0; i < kRepaintKickCount; ++i) {
    UINT delay = kRepaintKickMs * (i + 1);
    UINT_PTR tid = kRepaintKickTimerBase + i;
    LogFz("  arming kick %u (tid %llu) at +%ums", i + 1,
          static_cast<unsigned long long>(tid), delay);
    SetTimer(GetHandle(), tid, delay, nullptr);
  }

  return true;
}

bool FlutterWindow::OnCreate() {
  if (!Win32Window::OnCreate()) {
    return false;
  }
  LogFz("OnCreate");
  Show();
  shown_at_ = GetTickCount64();
  SetTimer(GetHandle(), kCreateViewTimer, kSettleMs, nullptr);
  return true;
}

void FlutterWindow::OnDestroy() {
  KillTimer(GetHandle(), kCreateViewTimer);
  KillTimer(GetHandle(), kRepaintKickTimer);
  for (unsigned i = 0; i < kRepaintKickCount; ++i) {
    KillTimer(GetHandle(), kRepaintKickTimerBase + i);
  }
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

    case WM_WINDOWPOSCHANGED: {
      if (kicking_) {
        break;
      }
      const auto* pos = reinterpret_cast<const WINDOWPOS*>(lparam);
      LogFz("WM_WINDOWPOSCHANGED %d,%d %dx%d flags=0x%X",
            pos ? pos->x : 0, pos ? pos->y : 0,
            pos ? pos->cx : 0, pos ? pos->cy : 0,
            pos ? pos->flags : 0);
      if (!flutter_started_ && GetTickCount64() - shown_at_ < kMaxSettleMs) {
        SetTimer(hwnd, kCreateViewTimer, kSettleMs, nullptr);
      }
      // Late zone move: re-arm one repaint kick so the engine re-presents at
      // the final size. (WM_TIMER handles this id too.)
      if (flutter_started_ && lparam != 0) {
        SetTimer(hwnd, kRepaintKickTimer, kRepaintKickMs, nullptr);
      }
      break;
    }

    case WM_SIZE: {
      if (kicking_) {
        break;
      }
      RECT rect = GetClientArea();
      LogFz("WM_SIZE %dx%d", rect.right - rect.left, rect.bottom - rect.top);
      if (flutter_started_) {
        // After any real size change, kick the view at the new size.
        SetTimer(hwnd, kRepaintKickTimer, kRepaintKickMs, nullptr);
      }
      break;
    }

    case WM_TIMER:
      if (wparam == kCreateViewTimer) {
        KillTimer(hwnd, kCreateViewTimer);
        LogFz("WM_TIMER create-view");
        if (!flutter_started_ && !CreateFlutterView()) {
          LogFz("  CreateFlutterView FAILED");
          DestroyWindow(hwnd);
        }
        return 0;
      }
      if (wparam == kRepaintKickTimer ||
          (wparam >= kRepaintKickTimerBase &&
           wparam < kRepaintKickTimerBase + kRepaintKickCount)) {
        KillTimer(hwnd, static_cast<UINT_PTR>(wparam));
        LogFz("WM_TIMER repaint-kick (tid %llu)",
              static_cast<unsigned long long>(wparam));
        KickRedraw();
        return 0;
      }
      break;
  }

  return Win32Window::MessageHandler(hwnd, message, wparam, lparam);
}
