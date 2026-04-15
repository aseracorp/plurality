import 'dart:convert';
import 'package:http/http.dart' as http;
import 'package:flutter/foundation.dart';
import '../auth/auth-service.dart';
import '../utils/types.dart';
import './balance.dart';
import './sse_event.dart';
import 'dart:io';

class ApiService {
  static final ApiService _instance = ApiService._internal();
  final AuthService _authService = AuthService();
  static final String baseUrl =
      kReleaseMode
          ? 'https://app.plurality-ai.com'
          : 'http://192.168.1.102:8090';

  // Factory constructor to return the same instance every time
  factory ApiService() {
    return _instance;
  }

  // Private constructor used by the factory constructor
  ApiService._internal();

  Future<void> CheckVerifyEmail(Function redirect) async {
    String? firebaseToken = await _authService.getCurrentUserToken();

    if (firebaseToken == null) {
      throw Exception('User not authenticated');
    }

    final response = await http.get(
      Uri.parse('$baseUrl/check'),
      headers: {'Authorization': 'Bearer $firebaseToken'},
    );

    if (response.statusCode == 412) {
      redirect();
    }
  }

  // Generic method to send authenticated POST requests
  Future<Map<String, dynamic>> sendPostRequest({
    required String endpoint,
    required Map<String, dynamic> body,
  }) async {
    try {
      // Get authentication token
      String? firebaseToken = await _authService.getCurrentUserToken();

      if (firebaseToken == null) {
        throw Exception('User not authenticated');
      }

      // Prepare and send the request
      final response = await http.post(
        Uri.parse('$baseUrl/$endpoint'),
        headers: {
          'Content-Type': 'application/json',
          'Authorization': 'Bearer $firebaseToken',
        },
        body: jsonEncode(body),
      );

      // Process the response
      if (response.statusCode >= 200 && response.statusCode < 300) {
        return jsonDecode(response.body);
      } else {
        throw Exception(
          'Request failed with status: ${response.statusCode}, message: ${response.reasonPhrase}',
        );
      }
    } catch (e) {
      throw Exception('API request failed: $e');
    }
  }

  /// Fetch raw bytes for an internal attachment URL (e.g. /attachments/uid/month/file.pdf).
  Future<Uint8List> fetchAttachmentBytes(String urlPath) async {
    String? firebaseToken = await _authService.getCurrentUserToken();
    if (firebaseToken == null) {
      throw Exception('User not authenticated');
    }

    final response = await http.get(
      Uri.parse('$baseUrl$urlPath'),
      headers: {'Authorization': 'Bearer $firebaseToken'},
    );

    if (response.statusCode >= 200 && response.statusCode < 300) {
      return response.bodyBytes;
    } else {
      throw Exception('Failed to fetch attachment: ${response.statusCode}');
    }
  }

  /// Search conversations via server-side FTS5 + vector search.
  /// Returns conversation IDs ranked by relevance.
  Future<List<String>> searchConversations(String query) async {
    String? firebaseToken = await _authService.getCurrentUserToken();
    if (firebaseToken == null) {
      throw Exception('User not authenticated');
    }

    final response = await http.get(
      Uri.parse('$baseUrl/search?q=${Uri.encodeQueryComponent(query)}'),
      headers: {'Authorization': 'Bearer $firebaseToken'},
    );

    if (response.statusCode >= 200 && response.statusCode < 300) {
      final List<dynamic> data = jsonDecode(response.body);
      return data.map((item) => item['conversation_id'] as String).toList();
    } else {
      return [];
    }
  }

  // --- Chat API Methods ---

  /// Parse an SSE response stream into a stream of SSEEvent objects.
  Stream<SSEEvent> _parseSSEStream(http.StreamedResponse response) {
    return response.stream
        .transform(utf8.decoder)
        .transform(const LineSplitter())
        .where((line) => line.startsWith('data: '))
        .map((line) => line.substring(6))
        .where((data) => data != '[DONE]')
        .map((data) {
          try {
            return SSEEvent.fromJson(jsonDecode(data));
          } catch (e) {
            throw APIException('JSON parsing error: $e');
          }
        });
  }

  /// Send a chat message or tool results. Returns a stream of SSE events.
  Future<Stream<SSEEvent>> postChat({
    required String conversationId,
    required ModelSelected modelSelected,
    List<Message>? messages,
    List<Message>? toolResults,
    MiniApp? miniApp,
    bool isCall = false,
    List<Map<String, dynamic>>? clientSideTools,
    List<String>? availableSkills,
  }) async {
    final request = http.Request('POST', Uri.parse('$baseUrl/chat'));
    request.headers['Content-Type'] = 'application/json';
    request.headers['Authorization'] =
        'Bearer ${await _authService.getCurrentUserToken()}';

    final body = <String, dynamic>{
      'conversation_id': conversationId,
      'model_selected': modelSelected.toJson(),
      'is_call': isCall,
    };
    if (messages != null) {
      body['messages'] = messages.map((m) => m.toJson()).toList();
    }
    if (toolResults != null) {
      body['tool_results'] = toolResults.map((m) => m.toJson()).toList();
    }
    if (miniApp != null) body['mini_app'] = miniApp.toJson();
    if (clientSideTools != null) body['client_side_tools'] = clientSideTools;
    if (availableSkills != null && availableSkills.isNotEmpty) {
      body['available_skills'] = availableSkills;
    }

    request.body = jsonEncode(body);

    final response = await request.send();
    if (response.statusCode != 200) {
      final errorBody = await response.stream.bytesToString();
      throw APIException(
        errorBody.isNotEmpty ? errorBody : 'Chat request failed',
        statusCode: response.statusCode,
      );
    }

    return _parseSSEStream(response);
  }

  /// Reconnect to an active conversation's SSE stream.
  Future<Stream<SSEEvent>> connectStream(String conversationId) async {
    final request = http.Request(
      'GET',
      Uri.parse('$baseUrl/chat/stream/$conversationId'),
    );
    request.headers['Authorization'] =
        'Bearer ${await _authService.getCurrentUserToken()}';

    final response = await request.send();
    if (response.statusCode != 200) {
      throw APIException(
        'No active stream for conversation',
        statusCode: response.statusCode,
      );
    }

    return _parseSSEStream(response);
  }

  /// Cancel an active chat request.
  Future<void> cancelChat(String conversationId) async {
    final token = await _authService.getCurrentUserToken();
    await http.post(
      Uri.parse('$baseUrl/chat/cancel/$conversationId'),
      headers: {'Authorization': 'Bearer $token'},
    );
  }

  /// Approve/deny server-side "ask" tools. Server executes approved ones,
  /// pushes results to DB, and relaunches the LLM loop with SSE streaming.
  Future<Stream<SSEEvent>> approveTools({
    required String conversationId,
    required List<Map<String, dynamic>> approvals,
    required ModelSelected modelSelected,
    List<Map<String, dynamic>>? clientSideTools,
    List<String>? availableSkills,
  }) async {
    final request = http.Request('POST', Uri.parse('$baseUrl/chat/approve/$conversationId'));
    request.headers['Content-Type'] = 'application/json';
    request.headers['Authorization'] =
        'Bearer ${await _authService.getCurrentUserToken()}';

    final body = <String, dynamic>{
      'approvals': approvals,
      'model_selected': modelSelected.toJson(),
    };
    if (clientSideTools != null) body['client_side_tools'] = clientSideTools;
    if (availableSkills != null && availableSkills.isNotEmpty) {
      body['available_skills'] = availableSkills;
    }

    request.body = jsonEncode(body);

    final response = await request.send();
    if (response.statusCode != 200) {
      final errorBody = await response.stream.bytesToString();
      throw APIException(
        errorBody.isNotEmpty ? errorBody : 'Approve request failed',
        statusCode: response.statusCode,
      );
    }

    return _parseSSEStream(response);
  }

  /// Fetch server-side tool metadata (icons, loading strings).
  Future<List<Map<String, dynamic>>> getServerTools() async {
    final token = await _authService.getCurrentUserToken();
    final response = await http.get(
      Uri.parse('$baseUrl/v1/tools'),
      headers: {'Authorization': 'Bearer $token'},
    );
    if (response.statusCode == 200) {
      return List<Map<String, dynamic>>.from(jsonDecode(response.body));
    }
    return [];
  }

  /// Connect to the global status stream (SSE). Returns lightweight StatusEvents
  /// for all active conversations — no content, just state changes.
  Future<Stream<Map<String, dynamic>>> connectStatusStream() async {
    final request = http.Request('GET', Uri.parse('$baseUrl/status/stream'));
    request.headers['Authorization'] =
        'Bearer ${await _authService.getCurrentUserToken()}';

    final response = await request.send();
    if (response.statusCode != 200) {
      throw APIException('Failed to connect status stream', statusCode: response.statusCode);
    }

    return response.stream
        .transform(utf8.decoder)
        .transform(const LineSplitter())
        .where((line) => line.startsWith('data: '))
        .map((line) => line.substring(6))
        .map((data) => jsonDecode(data) as Map<String, dynamic>);
  }

  Future<List<Conversation>> getConversations() async {
    try {
      // Get authentication token
      String? firebaseToken = await _authService.getCurrentUserToken();
      if (firebaseToken == null) {
        throw Exception('User not authenticated');
      }

      // Make the GET request
      final response = await http.get(
        Uri.parse('$baseUrl/conversations'),
        headers: {'Authorization': 'Bearer $firebaseToken'},
      );

      // Process the response
      if (response.statusCode >= 200 && response.statusCode < 300) {
        final decodedResponse = utf8.decode(response.bodyBytes);
        final List<dynamic> data = jsonDecode(decodedResponse);
        List<Conversation> conversations = [];
        for (var json in data) {
          try {
            conversations.add(Conversation.fromJson(json));
          } catch (e, s) {
            // Log or handle the error for the specific conversation
            print('Failed to parse conversation $json \n $e');
            print(s);
          }
        }
        return conversations;
      } else if (response.statusCode == 412) {
        throw APINeedEmailVerify();
      } else {
        throw APIException(
          'Failed to fetch conversations: ${response.reasonPhrase}',
          statusCode: response.statusCode,
        );
      }
    } catch (e) {
      if (e is APIException) rethrow;
      if (e is APINeedEmailVerify) rethrow;
      throw APIException('API request failed: $e');
    }
  }

  Future<void> deleteConversation(String conversationID) async {
    try {
      // Get authentication token
      String? firebaseToken = await _authService.getCurrentUserToken();
      if (firebaseToken == null) {
        throw Exception('User not authenticated');
      }

      // Make the DELETE request
      final response = await http.delete(
        Uri.parse('$baseUrl/conversation/$conversationID'),
        headers: {'Authorization': 'Bearer $firebaseToken'},
      );

      // Process the response
      if (response.statusCode >= 200 && response.statusCode < 300) {
        return;
      } else {
        throw APIException(
          'Failed to delete conversation: ${response.reasonPhrase}',
          statusCode: response.statusCode,
        );
      }
    } catch (e) {
      if (e is APIException) rethrow;
      throw APIException('API request failed: $e');
    }
  }

  // getConversation
  Future<Conversation?> getConversation(String conversationID) async {
    try {
      // Get authentication token
      String? firebaseToken = await _authService.getCurrentUserToken();
      if (firebaseToken == null) {
        throw Exception('User not authenticated');
      }
      // Make the GET request
      final response = await http.get(
        Uri.parse('$baseUrl/conversation/$conversationID'),
        headers: {'Authorization': 'Bearer $firebaseToken'},
      );
      // Process the response
      if (response.statusCode >= 200 && response.statusCode < 300) {
        // Use utf8.decode with response.bodyBytes instead of directly using response.body
        final decodedResponse = utf8.decode(response.bodyBytes);
        final json = jsonDecode(decodedResponse);
        return Conversation.fromJson(json);
      } else {
        throw APIException(
          'Failed to get conversation: ${response.reasonPhrase}',
          statusCode: response.statusCode,
        );
      }
    } catch (e) {
      if (e is APIException) rethrow;
      throw APIException('API request failed: $e');
    }
  }

  // getConversation

  Future<Map<String, dynamic>> generateTitle(String conversationID) async {
    try {
      // Get authentication token
      String? firebaseToken = await _authService.getCurrentUserToken();
      if (firebaseToken == null) {
        throw Exception('User not authenticated');
      }
      // Make the GET request
      final response = await http.get(
        Uri.parse('$baseUrl/generate-title/$conversationID'),
        headers: {'Authorization': 'Bearer $firebaseToken'},
      );
      // Process the response
      if (response.statusCode >= 200 && response.statusCode < 300) {
        // Use utf8.decode with response.bodyBytes instead of directly using response.body
        final decodedResponse = utf8.decode(response.bodyBytes);
        // decode json
        final json = jsonDecode(decodedResponse);

        return json;
      } else {
        throw APIException(
          'Failed to generate title for ${conversationID} : ${response.reasonPhrase}',
          statusCode: response.statusCode,
        );
      }
    } catch (e) {
      if (e is APIException) rethrow;
      throw APIException('API request failed: $e');
    }
  }

  Future<Balance> getBalance() async {
    try {
      // Get authentication token
      String? firebaseToken = await _authService.getCurrentUserToken();
      if (firebaseToken == null) {
        throw Exception('User not authenticated');
      }
      // Make the GET request
      final response = await http.get(
        Uri.parse('$baseUrl/balance'),
        headers: {'Authorization': 'Bearer $firebaseToken'},
      );
      // Process the response
      if (response.statusCode >= 200 && response.statusCode < 300) {
        final decodedResponse = utf8.decode(response.bodyBytes);
        final json = jsonDecode(decodedResponse);
        return Balance.fromJson(json);
      } else if (response.statusCode == 412) {
        throw APINeedEmailVerify();
      } else {
        throw APIException(
          'Failed to get balance: ${response.reasonPhrase}',
          statusCode: response.statusCode,
        );
      }
    } catch (e) {
      if (e is APIException) rethrow;
      if (e is APINeedEmailVerify) rethrow;
      throw APIException('API request failed: $e');
    }
  }

  // call DELETE /delete-user

  Future<void> deleteUser() async {
    try {
      // Get authentication token
      String? firebaseToken = await _authService.getCurrentUserToken();
      if (firebaseToken == null) {
        throw Exception('User not authenticated');
      }
      // Make the DELETE request
      final response = await http.delete(
        Uri.parse('$baseUrl/delete-user'),
        headers: {'Authorization': 'Bearer $firebaseToken'},
      );
      // Process the response
      if (response.statusCode >= 200 && response.statusCode < 300) {
        return;
      } else {
        throw APIException(
          'Failed to delete user: ${response.reasonPhrase}',
          statusCode: response.statusCode,
        );
      }
    } catch (e) {
      throw APIException('API request failed: $e');
    }
  }

  // Function to update conversation title
  Future<void> updateConversationTitle(
    String conversationID,
    String title,
  ) async {
    try {
      // Get authentication token
      String? firebaseToken = await _authService.getCurrentUserToken();
      if (firebaseToken == null) {
        throw Exception("User not authenticated");
      }

      // Prepare request body
      final requestBody = {"title": title};

      // Make the PUT request
      final response = await http.post(
        Uri.parse("$baseUrl/rename-conversation/$conversationID"),
        headers: {
          "Content-Type": "application/json",
          "Authorization": "Bearer $firebaseToken",
        },
        body: jsonEncode(requestBody),
      );

      // Process the response
      if (response.statusCode >= 200 && response.statusCode < 300) {
        return;
      } else {
        throw APIException(
          "Failed to update conversation title: ${response.reasonPhrase}",
          statusCode: response.statusCode,
        );
      }
    } catch (e) {
      if (e is APIException) rethrow;
      throw APIException("API request failed: $e");
    }
  }

  // Function to update conversation folder
  Future<void> updateConversationFolder(
    String conversationID,
    String folder,
  ) async {
    try {
      // Get authentication token
      String? firebaseToken = await _authService.getCurrentUserToken();
      if (firebaseToken == null) {
        throw Exception('User not authenticated');
      }

      // Prepare request body
      final requestBody = {'folder': folder};

      // Make the POST request (based on the Go code, this uses POST method)
      final response = await http.post(
        Uri.parse('$baseUrl/set-conversation-folder/$conversationID'),
        headers: {
          'Content-Type': 'application/json',
          'Authorization': 'Bearer $firebaseToken',
        },
        body: jsonEncode(requestBody),
      );

      // Process the response
      if (response.statusCode >= 200 && response.statusCode < 300) {
        return;
      } else {
        throw APIException(
          'Failed to update conversation folder: ${response.reasonPhrase}',
          statusCode: response.statusCode,
        );
      }
    } catch (e) {
      if (e is APIException) rethrow;
      throw APIException('API request failed: $e');
    }
  }

  Future<String> transcribeAudio(
    Uint8List audioData, {
    String model = "whisper-v3-turbo",
    String? language,
    double temperature = 0.0,
    String responseFormat = "text",
  }) async {
    try {
      // Get authentication token
      String? firebaseToken = await _authService.getCurrentUserToken();
      if (firebaseToken == null) {
        throw Exception('User not authenticated');
      }

      // Create the request
      final request = http.Request('POST', Uri.parse('$baseUrl/transcribe'));
      request.headers['Content-Type'] = 'application/json';
      request.headers['Authorization'] = 'Bearer $firebaseToken';

      // Prepare request body
      request.body = jsonEncode({
        'audioData': audioData,
        'model': model,
        'language': language,
        'temperature': temperature,
        'response_format': responseFormat,
      });

      // Send the request
      final response = await request.send();

      // Handle errors
      if (response.statusCode != 200) {
        final errorBody = await response.stream.bytesToString();
        String errorMessage;
        try {
          final errorJson = jsonDecode(errorBody);
          errorMessage = errorJson['error'] ?? 'Failed to transcribe audio';
        } catch (e) {
          errorMessage =
              errorBody.isNotEmpty ? errorBody : 'Failed to transcribe audio';
        }
        throw APIException(errorMessage, statusCode: response.statusCode);
      }

      // Parse the response
      try {
        return await response.stream.bytesToString();
      } catch (e) {
        throw APIException(
          'Error processing transcription response: ${e.toString()}',
        );
      }
    } catch (e) {
      if (e is APIException) rethrow;
      throw APIException('API request failed: $e');
    }
  }
}

class ImageResult {
  final String base64;
  final double time;
  ImageResult(this.base64, this.time);
}

class APINeedEmailVerify implements Exception {
  final String message = 'Email verification required';
}
