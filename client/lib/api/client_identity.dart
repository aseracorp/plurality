import 'dart:io' show Platform;
import 'package:flutter/foundation.dart'
    show kIsWeb, visibleForTesting;
import 'package:device_info_plus/device_info_plus.dart';
import 'package:uuid/uuid.dart';
import '../utils/types.dart' show ClientLock;

// Identity storage is scoped per TAB on web and per DEVICE on native.
// Conditional import, mirroring the repo's web_visibility pattern.
import 'client_scope_stub.dart'
    if (dart.library.html) 'client_scope_web.dart';

const String _identityKey = 'client_identity_id';

/// Stable identity for this client instance. Used by the client-lock feature:
/// a conversation that has been "locked" stores the lock holder's [id], and
/// other clients compare against their own [id] to decide whether to run
/// client-side tools.
///
/// Desktop: [id] and [label] both come from `Platform.localHostname` — that
/// matches the user's spec ("Client names are the hostname"). One process per
/// machine, so a durable per-device id is right.
///
/// Web: the id is persisted in **sessionStorage**, i.e. one identity per tab.
/// The old code persisted it through shared_preferences = localStorage, which
/// every tab of the browser shares, so two windows of the same browser were
/// indistinguishable: `lock.id != myId` was never true, both tabs claimed the
/// lock, both re-dispatched the same trailing tool calls and both drove their
/// own SSE reconnect. The per-process double-dispatch guards cannot see across
/// tabs, so the duplicated work froze the second window. Per-tab identity lets
/// the existing lock/banner design do its job: the holder runs the device-side
/// tools, the other tab shows the "locked on X" chip.
class ClientIdentity {
  static final ClientIdentity _instance = ClientIdentity._();
  factory ClientIdentity() => _instance;
  ClientIdentity._();

  String? _id;
  String? _label;
  Future<void>? _initFuture;

  Future<void> init() {
    return _initFuture ??= _init();
  }

  Future<void> _init() async {
    if (!kIsWeb &&
        (Platform.isWindows || Platform.isMacOS || Platform.isLinux)) {
      final host = Platform.localHostname;
      _id = host;
      _label = host;
      return;
    }

    var stableId = await readScopedId(_identityKey);
    if (stableId == null || stableId.isEmpty) {
      stableId = const Uuid().v4();
      await writeScopedId(_identityKey, stableId);
    }
    _id = stableId;
    final device = await _resolveDeviceLabel();
    // Two windows of one browser resolve to the same device label, so the
    // lock chip would be ambiguous; append a short stable per-tab tag.
    _label = kIsWeb ? '$device · ${_tabTag(stableId)}' : device;
  }

  /// Short stable suffix identifying one tab of a browser ("3f2a").
  String _tabTag(String id) =>
      id.replaceAll('-', '').substring(0, 4);

  /// Clears the memoised identity. Test-only: the singleton would otherwise
  /// leak state between cases.
  @visibleForTesting
  void resetForTest() {
    _id = null;
    _label = null;
    _initFuture = null;
  }

  Future<String> _resolveDeviceLabel() async {
    final info = DeviceInfoPlugin();
    try {
      if (kIsWeb) {
        final web = await info.webBrowserInfo;
        final name = web.browserName.name;
        return 'Web (${name.isEmpty ? 'browser' : name})';
      }
      if (Platform.isIOS) {
        final ios = await info.iosInfo;
        return ios.name;
      }
      if (Platform.isAndroid) {
        final android = await info.androidInfo;
        final base = android.model.isNotEmpty ? android.model : 'Android';
        return base;
      }
    } catch (_) {}
    return Platform.operatingSystem;
  }

  /// Stable opaque identifier for this client. Empty until [init] resolves.
  String get id => _id ?? '';

  /// Human-readable name shown in the "locked on X" banner on other
  /// clients. Empty until [init] resolves.
  String get label => _label ?? '';

  /// Convenience: build a [ClientLock] value pointing at this client.
  /// Returns null when identity hasn't initialised yet — callers should
  /// skip lock acquisition in that case rather than locking with an empty
  /// id.
  ClientLock? asLock() {
    final i = _id;
    final l = _label;
    if (i == null || i.isEmpty) return null;
    return ClientLock(id: i, label: l ?? i);
  }
}
