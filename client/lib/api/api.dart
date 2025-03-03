import 'dart:convert';
import 'dart:ffi';

import 'package:http/http.dart' as http;

import '../auth/auth-service.dart';
import '../utils/types.dart';

class ApiService {
  static final ApiService _instance = ApiService._internal();
  final AuthService _authService = AuthService();
  final String _baseUrl = 'http://localhost:8090';

  // Factory constructor to return the same instance every time
  factory ApiService() {
    return _instance;
  }

  // Private constructor used by the factory constructor
  ApiService._internal();

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
        Uri.parse('$_baseUrl/$endpoint'),
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

  // Specific method for chat functionality
  Future<Stream<String>> sendChatMessage(
    String conversationID,
    ModelSelected modelSelected,
    Message message,
    Function setMetaData,
  ) async {
    final request = http.Request('POST', Uri.parse(_baseUrl + '/chat'));
    request.headers['Content-Type'] = 'application/json';
    request.headers['Authorization'] =
        'Bearer ${await _authService.getCurrentUserToken()}';
    request.body = jsonEncode({
      'messages': [message],
      'conversation_id': conversationID,
      "model_selected": modelSelected,
    });

    final streamedResponse = await request.send();

    if (streamedResponse.statusCode != 200) {
      // Read the error message from the response
      final errorBody = await streamedResponse.stream.bytesToString();
      String errorMessage;
      try {
        final errorJson = jsonDecode(errorBody);
        errorMessage = errorJson['error'] ?? 'Unknown error occurred';
      } catch (e) {
        errorMessage =
            errorBody.isNotEmpty ? errorBody : 'Failed to send message';
      }
      throw APIException(errorMessage, statusCode: streamedResponse.statusCode);
    }

    return streamedResponse.stream
        .transform(utf8.decoder)
        .handleError((error) {
          throw APIException('Stream error: ${error.toString()}');
        })
        .transform(const LineSplitter())
        .handleError((error) {
          throw APIException('Line splitting error: ${error.toString()}');
        })
        .where(
          (line) =>
              line.startsWith('data: ') && !line.startsWith('data: [DONE]'),
        )
        .map((line) => line.substring(6))
        .map((line) {
          try {
            return jsonDecode(line);
          } catch (e) {
            throw APIException('JSON parsing error: ${e.toString()}');
          }
        })
        .map((json) {
          setMetaData(
            newConversationID: json['conversationID'] as String?,
            newConversationTitle: json['conversationTitle'] as String?,
          );
          final content = json['content'] as String?;
          if (content == null) {
            return '';
          }
          return content;
        });
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
        Uri.parse('$_baseUrl/conversations'),
        headers: {'Authorization': 'Bearer $firebaseToken'},
      );

      // Process the response
      if (response.statusCode >= 200 && response.statusCode < 300) {
        final List<dynamic> data = jsonDecode(response.body);
        return data.map((json) => Conversation.fromJson(json)).toList();
      } else {
        throw APIException(
          'Failed to fetch conversations: ${response.reasonPhrase}',
          statusCode: response.statusCode,
        );
      }
    } catch (e) {
      if (e is APIException) rethrow;
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
        Uri.parse('$_baseUrl/conversation/$conversationID'),
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

  Future<ImageResult> generateImage(String model, String prompt) async {
    final request = http.Request('POST', Uri.parse('$_baseUrl/generate-image'));
    request.headers['Content-Type'] = 'application/json';
    request.headers['Authorization'] =
        'Bearer ${await _authService.getCurrentUserToken()}';

    var steps = 40;

    if (model.contains("FLUX.1-schnell")) {
      steps = 12;
    }

    request.body = jsonEncode({
      'model': model,
      'prompt': prompt,
      'width': 1024,
      'height': 768,
      'steps': steps,
      'n': 1,
      'response_format': 'b64_json',
    });

    final response = await request.send();
    if (response.statusCode != 200) {
      final errorBody = await response.stream.bytesToString();
      String errorMessage;
      try {
        final errorJson = jsonDecode(errorBody);
        errorMessage = errorJson['error'] ?? 'Failed to generate image';
      } catch (e) {
        errorMessage =
            errorBody.isNotEmpty ? errorBody : 'Failed to generate image';
      }
      throw APIException(errorMessage, statusCode: response.statusCode);
    }

    try {
      final responseData = await response.stream.bytesToString();
      final jsonResponse = jsonDecode(responseData);
      if (jsonResponse['data']?.isEmpty ?? true) {
        throw APIException('No image data received');
      }
      var image64 = jsonResponse['data'][0]['b64_json'];
      var time = jsonResponse['data'][0]['timings']['inference'];
      time = (time * 1000).round() / 1000;
      return ImageResult(image64, time);
    } catch (e) {
      throw APIException('Error processing image response: ${e.toString()}');
    }
  }
}

class ImageResult {
  final String base64;
  final double time;
  ImageResult(this.base64, this.time);
}
