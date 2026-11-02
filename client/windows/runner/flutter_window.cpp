#include "flutter_window.h"

#include <optional>

#include "flutter/generated_plugin_registrant.h"

FlutterWindow::FlutterWindow(const flutter::DartProject& project)
    : project_(project) {}

FlutterWindow::~FlutterWindow() {}

bool FlutterWindow::CreateFlutterView() {
  if (flutter_started_) {
    return true;
  }

  RECT frame = GetClientArea();

  // The size here must match the window dimensions to avoid unnecessary
  // surface creation / destruction in the startup path.
  flutter_controller_ = std::make_unique<flutter::FlutterViewController>(
      frame.right - frame.left, frame.bottom - frame.top, project_);
  // Ensure that basic setup of the controller was successful.
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

  // Flutter can complete the first frame before the "show window" callback is
  // registered. The following call ensures a frame is pending to ensure the
  // window is shown. It is a no-op if the first frame hasn't completed yet.
  flutter_controller_->ForceRedraw();

  return true;
}

bool FlutterWindow::OnCreate() {
  if (!Win32Window::OnCreate()) {
    return false;
  }

  // PowerToys FancyZones (and other window managers) restore a window into
  // its last zone as soon as it appears, resizing it while Flutter is still
  // starting up. If that resize lands while the Flutter view is being created,
  // the view can end up sized for the wrong client area and never draw again
  // — the classic "snapped-to-a-zone => blank window" symptom.
  //
  // Fix: show the window immediately (empty, background-coloured frame), then
  // start Flutter only after the window stops moving. Each WM_WINDOWPOSCHANGED
  // while the window is still settling re-arms the timer, so the view is
  // created with the *final* zone size. Bounded by kMaxSettleMs so a window
  // that keeps moving can't delay startup forever.
  Show();
  shown_at_ = GetTickCount64();
  SetTimer(GetHandle(), kCreateViewTimer, kSettleMs, nullptr);

  return true;
}

void FlutterWindow::OnDestroy() {
  KillTimer(GetHandle(), kCreateViewTimer);
  if (flutter_controller_) {
    flutter_controller_ = nullptr;
  }

  Win32Window::OnDestroy();
}

LRESULT
FlutterWindow::MessageHandler(HWND hwnd, UINT const message,
                              WPARAM const wparam,
                              LPARAM const lparam) noexcept {
  // Give Flutter, including plugins, an opportunity to handle window messages.
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

    case WM_WINDOWPOSCHANGED:
      // Still waiting to start Flutter: keep deferring while the window is
      // moving (zone restore / user drag). Create the view shortly after it
      // stops, bounded by kMaxSettleMs from when the window was shown.
      if (!flutter_started_ && GetTickCount64() - shown_at_ < kMaxSettleMs) {
        SetTimer(hwnd, kCreateViewTimer, kSettleMs, nullptr);
      }
      break;

    case WM_TIMER:
      if (wparam == kCreateViewTimer) {
        KillTimer(hwnd, kCreateViewTimer);
        if (!flutter_started_ && !CreateFlutterView()) {
          DestroyWindow(hwnd);
        }
        return 0;
      }
      break;
  }

  return Win32Window::MessageHandler(hwnd, message, wparam, lparam);
}
