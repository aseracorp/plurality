#ifndef RUNNER_FLUTTER_WINDOW_H_
#define RUNNER_FLUTTER_WINDOW_H_

#include <flutter/dart_project.h>
#include <flutter/flutter_view_controller.h>

#include <memory>

#include "win32_window.h"

// A window that does nothing but host a Flutter view.
class FlutterWindow : public Win32Window {
 public:
  // Creates a new FlutterWindow hosting a Flutter view running |project|.
  explicit FlutterWindow(const flutter::DartProject& project);
  virtual ~FlutterWindow();

 protected:
  // Win32Window:
  bool OnCreate() override;
  void OnDestroy() override;
  LRESULT MessageHandler(HWND window, UINT const message, WPARAM const wparam,
                         LPARAM const lparam) noexcept override;

 private:
  // The project to run.
  flutter::DartProject project_;

  // The Flutter instance hosted by this window.
  std::unique_ptr<flutter::FlutterViewController> flutter_controller_;

  // True once the Flutter view has been created (guards CreateFlutterView
  // so the deferred-startup timer can never double-create it).
  bool flutter_started_ = false;

  // Wall-clock time (GetTickCount64) the window was first shown. Used to
  // bound how long the deferred view startup may be held back by window
  // manager movement (PowerToys FancyZones).
  ULONGLONG shown_at_ = 0;

  // Creates the FlutterViewController and registers plugins. Must only be
  // called once; safe to call from a WM_TIMER handler or OnCreate.
  bool CreateFlutterView();

  // Deferred-startup timer: create the Flutter view shortly after the window
  // stops moving, so FancyZones' zone restore can't race the view creation.
  static constexpr UINT_PTR kCreateViewTimer = 1;
  static constexpr UINT kSettleMs = 150;
  static constexpr ULONGLONG kMaxSettleMs = 1000;
};

#endif  // RUNNER_FLUTTER_WINDOW_H_
