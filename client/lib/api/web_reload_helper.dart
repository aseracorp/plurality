// lib/src/helpers/web_reload_helper.dart

// Import the web package ONLY in this file
import 'package:web/web.dart' as web;

// Define the function that performs the web-specific action
void platformSpecificReload() {
  web.window.location.reload();
}
