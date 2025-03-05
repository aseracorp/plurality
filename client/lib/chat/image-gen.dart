import '../utils/types.dart';
import '../api/api.dart';
import '../api/service.dart';

Function genImage = (
  String model,
  String currentStreamedResponse,
  String currentConversationID,
  ConversationsNotifier conversationsNotifier,
  ApiService apiService,
) async {
  final imageMatch = RegExp(
    r'/image ([^\n]+)',
  ).firstMatch(currentStreamedResponse);

  if (imageMatch != null) {
    final imageMessage = await apiService.generateImage(
      currentConversationID,
      model,
      imageMatch.group(1)!,
    );

    // Add assistant's image response to Riverpod state
    conversationsNotifier.addMessage(
      conversationId: currentConversationID,
      message: imageMessage,
    );
  }
};
