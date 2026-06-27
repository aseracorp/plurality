// Conditional re-export: picks the IO implementation on native platforms
// (dart:io available) and the browser implementation on web (dart:html).
export 'openid_result.dart';
export 'openid_signin_stub.dart'
    if (dart.library.io) 'openid_signin_io.dart'
    if (dart.library.html) 'openid_signin_web.dart';
