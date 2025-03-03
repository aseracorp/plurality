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
    final imageResult = await apiService.generateImage(
      model,
      imageMatch.group(1)!,
    );

    final imageMessage = Message(
      role: "assistant",
      content: [
        MessageContent.image(imageResult.base64),
        MessageContent.text('Generated image in ${imageResult.time}s'),
      ],
    );

    // Add assistant's image response to Riverpod state
    conversationsNotifier.addMessage(
      conversationId: currentConversationID,
      message: imageMessage,
    );
  }
};
