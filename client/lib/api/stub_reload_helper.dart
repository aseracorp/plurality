// lib/src/helpers/stub_reload_helper.dart

// DO NOT import 'package:web' here

// Define the function with the same name, but do nothing (or log/throw)
void platformSpecificReload() {
  // On non-web platforms, reload doesn't make sense in the same way.
  // You can leave this empty, log a message, or throw an exception
  // if it's ever called inappropriately.
  print("Page reload requested on non-web platform. Doing nothing.");
  // Or: throw UnimplementedError('Page reload is only available on web.');
}
