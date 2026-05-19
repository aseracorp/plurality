import 'dart:io' show Platform;
import 'package:flutter/foundation.dart' show kIsWeb;
import 'package:device_info_plus/device_info_plus.dart';
import 'package:shared_preferences/shared_preferences.dart';
import 'package:uuid/uuid.dart';
import '../utils/types.dart' show ClientLock;

/// Stable identity for this physical client (machine / device / browser
/// install). Used by the client-lock feature: a conversation that has been
/// "locked" stores the lock holder's [id], and other clients compare
/// against their own [id] to decide whether to run client-side tools.
///
/// Desktop: [id] and [label] both come from `Platform.localHostname` — that
/// matches the user's spec ("Client names are the hostname"). On mobile and
/// web, the hostname isn't meaningful, so we mint a UUID once and persist
/// it in shared_preferences; the label comes from device_info_plus.
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

    final prefs = await SharedPreferences.getInstance();
    var stableId = prefs.getString('client_identity_id');
    if (stableId == null || stableId.isEmpty) {
      stableId = const Uuid().v4();
      await prefs.setString('client_identity_id', stableId);
    }
    _id = stableId;
    _label = await _resolveDeviceLabel();
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
