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

  // True while the window is in startup lockdown: external window managers
  // (PowerToys FancyZones) have their WM_WINDOWPOSCHANGING neutralized so the
  // window starts at its own saved size, renders its first frames, and only
  // then becomes freely draggable/snappable.
  bool startup_locked_ = false;

  // Persists the current window rect to HKCU\\Software\\AseraCorp\\Plurality
  // (called on destroy) and restores it in OnCreate so the app always starts
  // at the size/position it was closed with, regardless of FancyZones.
  void SaveSizeToRegistry();
  void LoadSavedSize();

  // Creates the FlutterViewController and registers plugins. Must only be
  // called once. Returns false only on a permanent failure (caller destroys
  // the window).
  bool CreateFlutterView();

  // Appends a line to %TEMP%\\plurality_fancyzones.log for diagnosing the
  // FancyZones blank-window bug. No-op if logging is disabled at build.
  void LogFz(const char* fmt, ...);

  // Startup-lock timer: after kStartupLockMs the window is fully
  // draggable/snappable into any FancyZones zone.
  static constexpr UINT_PTR kStartupUnlockTimer = 4;
  static constexpr ULONGLONG kStartupLockMs = 4000;
};

#endif  // RUNNER_FLUTTER_WINDOW_H_
