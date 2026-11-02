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
  // bound how long the deferred view startup / repaint kick may be held
  // back by window-manager movement (PowerToys FancyZones).
  ULONGLONG shown_at_ = 0;

  // True while KickRedraw() is applying its synthetic resize. The resulting
  // WM_WINDOWPOSCHANGED / WM_SIZE are ignored for re-arming, so the nudge
  // can't trigger an endless kick loop.
  bool kicking_ = false;

  // True while the window is in startup lockdown: external window managers
  // (PowerToys FancyZones) have their WM_WINDOWPOSCHANGING rejected so the
  // window starts at its own saved size, renders its first frames, and only
  // then becomes freely draggable/snappable.
  bool startup_locked_ = false;

  // Persists the current window rect to HKCU\Software\AseraCorp\Plurality
  // (called on destroy) and restores it in OnCreate so the app always starts
  // at the size/position it was closed with, regardless of FancyZones.
  void SaveSizeToRegistry();
  void LoadSavedSize();

  // Creates the FlutterViewController and registers plugins. Must only be
  // called once. Returns true when the view is up (or a retry was re-armed);
  // returns false only on a permanent failure (caller destroys the window).
  bool CreateFlutterView();

  // Forces a clean, deterministic redraw of the Flutter view at the current
  // client size. On FancyZones setups the engine's swapchain present can
  // silently fail during start-up moves; a plain ForceRedraw is sometimes not
  // enough, so this also re-positions the child view to the exact client rect
  // and invalidates the parent (mimicking the manual resize that recovers it).
  void KickRedraw();

  // Appends a line to %TEMP%\plurality_fancyzones.log for diagnosing the
  // FancyZones blank-window bug. No-op if logging is disabled at build.
  void LogFz(const char* fmt, ...);

  // Deferred-startup timer: create the Flutter view shortly after the window
  // stops moving. PowerToys FancyZones animates the window into its zone over
  // several seconds (the long 0x80A15 WM_WINDOWPOSCHANGED series), so a short
  // cap races the animation. Instead: wait for quiescence (kSettleMs without
  // any move), bounded only by kMaxStartupMs as a safety net so a window that
  // keeps moving forever can't block startup forever.
  static constexpr UINT_PTR kCreateViewTimer = 1;
  static constexpr UINT_PTR kMaxStartupTimer = 3;
  static constexpr UINT kSettleMs = 250;
  static constexpr ULONGLONG kMaxStartupMs = 10000;

  // Repaint-kick timers: after the first frame (and after late window-manager
  // moves), force one clean redraw so a silently-failed swapchain present is
  // recovered without requiring the user to resize the window. Several
  // staggered kicks (distinct timer ids!) cover moves that land mid-startup.
  static constexpr UINT_PTR kRepaintKickTimer = 2;
  static constexpr UINT_PTR kStartupUnlockTimer = 4;
  static constexpr ULONGLONG kStartupLockMs = 4000;  // hold external geometry off this long
  static constexpr UINT_PTR kRepaintKickTimerBase = 10;
  static constexpr UINT kRepaintKickCount = 6;
  static constexpr UINT kRepaintKickMs = 250;
};

#endif  // RUNNER_FLUTTER_WINDOW_H_
