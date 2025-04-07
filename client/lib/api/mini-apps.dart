import 'dart:convert';
import 'package:http/http.dart' as http;
import 'package:flutter/foundation.dart';
import '../auth/auth-service.dart';
import '../utils/types.dart';

class MiniAppsService {
  static final MiniAppsService _instance = MiniAppsService._internal();
  final AuthService _authService = AuthService();
  final String _baseUrl =
      kReleaseMode
          ? 'https://app.plurality-ai.com'
          : 'http://192.168.1.102:8090';

  // Factory constructor to return the same instance every time
  factory MiniAppsService() {
    return _instance;
  }

  // Private constructor used by the factory constructor
  MiniAppsService._internal();

  // Get all available mini-apps
  Future<List<MiniApp>> getAllMiniApps() async {
    try {
      // Get authentication token
      String? firebaseToken = await _authService.getCurrentUserToken();
      if (firebaseToken == null) {
        throw Exception('User not authenticated');
      }

      // Make the GET request
      final response = await http.get(
        Uri.parse('$_baseUrl/miniapps'),
        headers: {'Authorization': 'Bearer $firebaseToken'},
      );

      // Process the response
      if (response.statusCode >= 200 && response.statusCode < 300) {
        final decodedResponse = utf8.decode(response.bodyBytes);
        final List<dynamic> data = jsonDecode(decodedResponse);
        List<MiniApp> miniApps = [];
        for (var json in data) {
          try {
            miniApps.add(MiniApp.fromJson(json));
          } catch (e, stacktrace) {
            // Log or handle the error for the specific mini-app
            print('Failed to parse mini-app $json \n $e');
            // stacktrace can be accessed using e.stackTrace
            print(stacktrace);
          }
        }
        return miniApps;
      } else if (response.statusCode == 412) {
        throw APINeedEmailVerify();
      } else {
        throw APIException(
          'Failed to fetch mini-apps: ${response.reasonPhrase}',
          statusCode: response.statusCode,
        );
      }
    } catch (e) {
      if (e is APIException) rethrow;
      if (e is APINeedEmailVerify) rethrow;
      throw APIException('API request failed: $e');
    }
  }

  // Get user's pinned mini-apps
  Future<List<MiniApp>> getUserPinnedMiniApps() async {
    try {
      // Get authentication token
      String? firebaseToken = await _authService.getCurrentUserToken();
      if (firebaseToken == null) {
        throw Exception('User not authenticated');
      }

      // Make the GET request
      final response = await http.get(
        Uri.parse('$_baseUrl/miniapps/pinned'),
        headers: {'Authorization': 'Bearer $firebaseToken'},
      );

      // Process the response
      if (response.statusCode >= 200 && response.statusCode < 300) {
        final decodedResponse = utf8.decode(response.bodyBytes);
        final List<dynamic> data = jsonDecode(decodedResponse);
        List<MiniApp> pinnedMiniApps = [];
        for (var json in data) {
          try {
            pinnedMiniApps.add(MiniApp.fromJson(json));
          } catch (e) {
            // Log or handle the error for the specific mini-app
            print('Failed to parse pinned mini-app $json \n $e');
          }
        }
        return pinnedMiniApps;
      } else if (response.statusCode == 412) {
        throw APINeedEmailVerify();
      } else {
        throw APIException(
          'Failed to fetch pinned mini-apps: ${response.reasonPhrase}',
          statusCode: response.statusCode,
        );
      }
    } catch (e) {
      if (e is APIException) rethrow;
      if (e is APINeedEmailVerify) rethrow;
      throw APIException('API request failed: $e');
    }
  }

  // Get a specific mini-app by ID
  Future<MiniApp> getMiniAppById(String miniAppId) async {
    try {
      // Get authentication token
      String? firebaseToken = await _authService.getCurrentUserToken();
      if (firebaseToken == null) {
        throw Exception('User not authenticated');
      }

      // Make the GET request
      final response = await http.get(
        Uri.parse('$_baseUrl/miniapps/$miniAppId'),
        headers: {'Authorization': 'Bearer $firebaseToken'},
      );

      // Process the response
      if (response.statusCode >= 200 && response.statusCode < 300) {
        final decodedResponse = utf8.decode(response.bodyBytes);
        final json = jsonDecode(decodedResponse);
        return MiniApp.fromJson(json);
      } else {
        throw APIException(
          'Failed to get mini-app: ${response.reasonPhrase}',
          statusCode: response.statusCode,
        );
      }
    } catch (e) {
      if (e is APIException) rethrow;
      throw APIException('API request failed: $e');
    }
  }

  // Create a new mini-app
  Future<MiniApp> createMiniApp(MiniApp miniApp) async {
    try {
      // Get authentication token
      String? firebaseToken = await _authService.getCurrentUserToken();
      if (firebaseToken == null) {
        throw Exception('User not authenticated');
      }

      // Make the POST request
      final response = await http.post(
        Uri.parse('$_baseUrl/miniapps'),
        headers: {
          'Content-Type': 'application/json',
          'Authorization': 'Bearer $firebaseToken',
        },
        body: jsonEncode(miniApp.toJson()),
      );

      // Process the response
      if (response.statusCode >= 200 && response.statusCode < 300) {
        final decodedResponse = utf8.decode(response.bodyBytes);
        final json = jsonDecode(decodedResponse);
        return MiniApp.fromJson(json);
      } else {
        throw APIException(
          'Failed to create mini-app: ${response.reasonPhrase}',
          statusCode: response.statusCode,
        );
      }
    } catch (e) {
      if (e is APIException) rethrow;
      throw APIException('API request failed: $e');
    }
  }

  // Update an existing mini-app
  Future<MiniApp> updateMiniApp(String miniAppId, MiniApp miniApp) async {
    try {
      // Get authentication token
      String? firebaseToken = await _authService.getCurrentUserToken();
      if (firebaseToken == null) {
        throw Exception('User not authenticated');
      }

      // Make the PUT request
      final response = await http.put(
        Uri.parse('$_baseUrl/miniapps/$miniAppId'),
        headers: {
          'Content-Type': 'application/json',
          'Authorization': 'Bearer $firebaseToken',
        },
        body: jsonEncode(miniApp.toJson()),
      );

      // Process the response
      if (response.statusCode >= 200 && response.statusCode < 300) {
        final decodedResponse = utf8.decode(response.bodyBytes);
        final json = jsonDecode(decodedResponse);
        return MiniApp.fromJson(json);
      } else {
        throw APIException(
          'Failed to update mini-app: ${response.reasonPhrase}',
          statusCode: response.statusCode,
        );
      }
    } catch (e) {
      if (e is APIException) rethrow;
      throw APIException('API request failed: $e');
    }
  }

  // Delete a mini-app
  Future<void> deleteMiniApp(String miniAppId) async {
    try {
      // Get authentication token
      String? firebaseToken = await _authService.getCurrentUserToken();
      if (firebaseToken == null) {
        throw Exception('User not authenticated');
      }

      // Make the DELETE request
      final response = await http.delete(
        Uri.parse('$_baseUrl/miniapps/$miniAppId'),
        headers: {'Authorization': 'Bearer $firebaseToken'},
      );

      // Process the response
      if (response.statusCode >= 200 && response.statusCode < 300) {
        return;
      } else {
        throw APIException(
          'Failed to delete mini-app: ${response.reasonPhrase}',
          statusCode: response.statusCode,
        );
      }
    } catch (e) {
      if (e is APIException) rethrow;
      throw APIException('API request failed: $e');
    }
  }

  // Pin a mini-app for the current user
  Future<void> pinMiniApp(String miniAppId) async {
    try {
      // Get authentication token
      String? firebaseToken = await _authService.getCurrentUserToken();
      if (firebaseToken == null) {
        throw Exception('User not authenticated');
      }

      // Make the POST request
      final response = await http.post(
        Uri.parse('$_baseUrl/miniapps/$miniAppId/pin'),
        headers: {'Authorization': 'Bearer $firebaseToken'},
      );

      // Process the response
      if (response.statusCode >= 200 && response.statusCode < 300) {
        return;
      } else {
        throw APIException(
          'Failed to pin mini-app: ${response.reasonPhrase}',
          statusCode: response.statusCode,
        );
      }
    } catch (e) {
      if (e is APIException) rethrow;
      throw APIException('API request failed: $e');
    }
  }

  // Unpin a mini-app for the current user
  Future<void> unpinMiniApp(String miniAppId) async {
    try {
      // Get authentication token
      String? firebaseToken = await _authService.getCurrentUserToken();
      if (firebaseToken == null) {
        throw Exception('User not authenticated');
      }

      // Make the POST request
      final response = await http.post(
        Uri.parse('$_baseUrl/miniapps/$miniAppId/unpin'),
        headers: {'Authorization': 'Bearer $firebaseToken'},
      );

      // Process the response
      if (response.statusCode >= 200 && response.statusCode < 300) {
        return;
      } else {
        throw APIException(
          'Failed to unpin mini-app: ${response.reasonPhrase}',
          statusCode: response.statusCode,
        );
      }
    } catch (e) {
      if (e is APIException) rethrow;
      throw APIException('API request failed: $e');
    }
  }

  // Use a mini-app to create a conversation
  Future<Conversation> useMiniApp(
    String miniAppId,
    Map<String, dynamic> formInputs,
  ) async {
    try {
      // Get authentication token
      String? firebaseToken = await _authService.getCurrentUserToken();
      if (firebaseToken == null) {
        throw Exception('User not authenticated');
      }

      // Make the POST request
      final response = await http.post(
        Uri.parse('$_baseUrl/miniapps/$miniAppId/use'),
        headers: {
          'Content-Type': 'application/json',
          'Authorization': 'Bearer $firebaseToken',
        },
        body: jsonEncode(formInputs),
      );

      // Process the response
      if (response.statusCode >= 200 && response.statusCode < 300) {
        final decodedResponse = utf8.decode(response.bodyBytes);
        final json = jsonDecode(decodedResponse);
        return Conversation.fromJson(json);
      } else {
        throw APIException(
          'Failed to use mini-app: ${response.reasonPhrase}',
          statusCode: response.statusCode,
        );
      }
    } catch (e) {
      if (e is APIException) rethrow;
      throw APIException('API request failed: $e');
    }
  }
}

class APIException implements Exception {
  final String message;
  final int statusCode;

  APIException(this.message, {this.statusCode = 0});

  @override
  String toString() => 'APIException: $message (Status code: $statusCode)';
}

class APINeedEmailVerify implements Exception {
  final String message = 'Email verification required';
}
