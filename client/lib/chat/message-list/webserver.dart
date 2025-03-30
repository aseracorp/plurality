import 'dart:async';
import 'dart:io';
import 'dart:math';
import 'package:flutter/material.dart';
import 'package:url_launcher/url_launcher.dart';

class StaticWebServer {
  static final StaticWebServer _instance = StaticWebServer._internal();
  factory StaticWebServer() => _instance;

  HttpServer? _server;
  int? _port;
  final Map<String, String> _pageCache = {};

  // Base URL for the server
  String get baseUrl => 'http://localhost:$_port';

  StaticWebServer._internal();

  // Initialize the server
  Future<void> initialize() async {
    if (_server != null) return; // Already initialized

    _port = await _findRandomFreePort();
    _server = await HttpServer.bind(InternetAddress.loopbackIPv4, _port!);

    debugPrint('Static web server started on port $_port');

    // Handle incoming requests
    _server!.listen((HttpRequest request) {
      _handleRequest(request);
    });
  }

  // Find a random free port
  Future<int> _findRandomFreePort() async {
    final random = Random();
    int port;
    ServerSocket? socket;

    // Try ports in range 8000-9000
    do {
      port = 8000 + random.nextInt(1000);
      try {
        socket = await ServerSocket.bind(
          InternetAddress.loopbackIPv4,
          port,
          shared: true,
        );
        await socket.close();
        return port;
      } catch (e) {
        // Port is in use, try another one
      }
    } while (true);
  }

  // Add a page to the cache
  String addPage(String content, {String? path}) {
    path ??= '/page/${DateTime.now().millisecondsSinceEpoch}';
    if (!path.startsWith('/')) path = '/$path';

    _pageCache[path] = content;
    return '$baseUrl$path';
  }

  // Handle HTTP requests
  void _handleRequest(HttpRequest request) {
    final path = request.uri.path;

    if (path == '/' || path.isEmpty) {
      // Serve index page with links to all available pages
      _serveIndexPage(request);
    } else if (_pageCache.containsKey(path)) {
      // Serve cached page
      _serveCachedPage(request, path);
    } else {
      // Page not found
      _serveNotFoundPage(request);
    }
  }

  // Serve the index page
  void _serveIndexPage(HttpRequest request) {
    final response = request.response;
    response.headers.contentType = ContentType.html;

    final buffer = StringBuffer();
    buffer.write('''
    <!DOCTYPE html>
    <html>
    <head>
      <title>Static Web Server</title>
      <meta name="viewport" content="width=device-width, initial-scale=1">
      <style>
        body {
          font-family: Arial, sans-serif;
          max-width: 800px;
          margin: 0 auto;
          padding: 20px;
        }
        h1 {
          color: #333;
        }
        ul {
          list-style-type: none;
          padding: 0;
        }
        li {
          margin-bottom: 10px;
        }
        a {
          color: #0066cc;
          text-decoration: none;
        }
        a:hover {
          text-decoration: underline;
        }
      </style>
    </head>
    <body>
      <h1>Available Pages</h1>
    ''');

    if (_pageCache.isEmpty) {
      buffer.write('<p>No pages available</p>');
    } else {
      buffer.write('<ul>');
      _pageCache.keys.forEach((path) {
        buffer.write('<li><a href="$path">$path</a></li>');
      });
      buffer.write('</ul>');
    }

    buffer.write('''
    </body>
    </html>
    ''');

    response.write(buffer.toString());
    response.close();
  }

  // Serve a cached page
  void _serveCachedPage(HttpRequest request, String path) {
    final response = request.response;
    response.headers.contentType = ContentType.html;
    response.write(_pageCache[path]);
    response.close();
  }

  // Serve a 404 page
  void _serveNotFoundPage(HttpRequest request) {
    final response = request.response;
    response.statusCode = HttpStatus.notFound;
    response.headers.contentType = ContentType.html;

    response.write('''
    <!DOCTYPE html>
    <html>
    <head>
      <title>404 - Page Not Found</title>
      <meta name="viewport" content="width=device-width, initial-scale=1">
      <style>
        body {
          font-family: Arial, sans-serif;
          max-width: 800px;
          margin: 0 auto;
          padding: 20px;
          text-align: center;
        }
        h1 {
          color: #d9534f;
        }
        a {
          color: #0066cc;
          text-decoration: none;
        }
        a:hover {
          text-decoration: underline;
        }
      </style>
    </head>
    <body>
      <h1>404 - Page Not Found</h1>
      <p>The requested page does not exist.</p>
      <p><a href="/">Back to Home</a></p>
    </body>
    </html>
    ''');

    response.close();
  }

  // Stop the server
  Future<void> stop() async {
    if (_server != null) {
      await _server!.close();
      _server = null;
      _port = null;
      _pageCache.clear();
      debugPrint('Static web server stopped');
    }
  }
}
