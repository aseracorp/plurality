// import 'dart:async';
// import 'dart:typed_data';
// import 'package:flutter/foundation.dart';
// import 'package:just_audio/just_audio.dart';
// import 'api.dart';

// // First, create a custom StreamAudioSource implementation
// class ByteStreamAudioSource extends StreamAudioSource {
//   final Stream<Uint8List> _byteStream;
//   final String _mimeType;

//   ByteStreamAudioSource(this._byteStream, {String mimeType = 'audio/mp3'})
//     : _mimeType = mimeType;

//   @override
//   Future<StreamAudioResponse> request([int? start, int? end]) async {
//     // Create a controller to manage the stream
//     final controller = StreamController<List<int>>();

//     // Subscribe to the byte stream and add data to the controller
//     final subscription = _byteStream.listen(
//       (data) => controller.add(data),
//       onError: controller.addError,
//       onDone: controller.close,
//     );

//     // When the stream is closed, cancel the subscription
//     controller.onCancel = () {
//       subscription.cancel();
//     };

//     // Return a StreamAudioResponse with the stream
//     return StreamAudioResponse(
//       sourceLength: null, // Length is unknown for streaming
//       contentLength: null, // Content length is unknown for streaming
//       offset: start ?? 0,
//       stream: controller.stream,
//       contentType: _mimeType,
//     );
//   }
// }

// class TTSService {
//   // Singleton pattern
//   static final TTSService _instance = TTSService._internal();
//   factory TTSService() => _instance;
//   TTSService._internal();

//   final AudioPlayer _audioPlayer = AudioPlayer();
//   bool _isSpeaking = false;

//   Function? cb;

//   // Stream controller to notify listeners about speaking state changes
//   final _speakingStateController = StreamController<bool>.broadcast();
//   Stream<bool> get speakingState => _speakingStateController.stream;

//   // Current text being spoken
//   String? _currentText;
//   String? get currentText => _currentText;

//   // Initialize the TTS service
//   Future<void> initialize() async {
//     _audioPlayer.playerStateStream.listen((state) {
//       if (state.processingState == ProcessingState.completed) {
//         _isSpeaking = false;
//         _currentText = null;
//         _speakingStateController.add(false);
//       }
//     });
//   }

//   // Check if TTS is currently speaking
//   bool get isSpeaking => _isSpeaking;

//   // Extract plain text from markdown
//   String _extractPlainText(String markdownText) {
//     return markdownText
//         .replaceAll(RegExp(r'```[\s\S]*?```'), '') // Remove code blocks
//         .replaceAll(RegExp(r'`[^`]*`'), '') // Remove inline code
//         .replaceAll(RegExp(r'!\[.*?\]\(.*?\)'), '') // Remove images
//         .replaceAll(
//           RegExp(r'\[([^\]]*)\]\([^)]*\)'),
//           r'$1',
//         ) // Replace links with just their text
//         .replaceAll(RegExp(r'#+\s+'), '') // Remove heading markers
//         .replaceAll(
//           RegExp(r'\*\*|\*|__|\|'),
//           '',
//         ) // Remove bold, italic, table markers
//         .replaceAll(RegExp(r'>\s+'), '') // Remove blockquote markers
//         .replaceAll(RegExp(r'={2,}'), '=') // Remove blockquote markers
//         .replaceAll(RegExp(r'- |\d+\. '), '') // Remove list markers
//         .trim();
//   }

//   List<String> speakQueue = [];

//   // Speak text using the audio generation API
//   Future<void> speak(
//     String markdownText,
//     Function? _cb, {
//     bool queue = false,
//   }) async {
//     if (markdownText.isEmpty) return;

//     // Stop any current speech
//     if (_isSpeaking) {
//       if (queue) {
//         speakQueue.add(markdownText);
//         return;
//       } else {
//         await stop();
//       }
//     }

//     if (_cb != null) {
//       cb = _cb;
//     }

//     String plainText = _extractPlainText(markdownText);

//     _isSpeaking = true;
//     _currentText = markdownText;
//     _speakingStateController.add(true);

//     try {
//       // Get the audio stream from your API
//       final audioStream = await ApiService().generateAudio(
//         text: plainText,
//         voice: "indian lady", // Replace with desired voice
//       );

//       // Create a custom StreamAudioSource with your byte stream
//       // Adjust the MIME type to match what your API returns (mp3, wav, etc.)
//       final audioSource = ByteStreamAudioSource(
//         audioStream,
//         mimeType: 'audio/mp3',
//       );

//       // Set the audio source and play
//       await _audioPlayer.setAudioSource(audioSource);
//       await _audioPlayer.play();

//       // Handle queued items
//       while (speakQueue.isNotEmpty) {
//         String nextText = speakQueue.first;
//         speakQueue.removeAt(0);
//         _currentText = nextText;

//         final nextAudioStream = await ApiService().generateAudio(
//           text: _extractPlainText(nextText),
//           voice: "indian lady", // Replace with desired voice
//         );

//         final nextAudioSource = ByteStreamAudioSource(
//           nextAudioStream,
//           mimeType: 'audio/mp3',
//         );
//         await _audioPlayer.setAudioSource(nextAudioSource);
//         await _audioPlayer.play();
//       }
//     } catch (e) {
//       print('Error during TTS playback: $e');
//       _isSpeaking = false;
//       _currentText = null;
//       _speakingStateController.add(false);
//     }

//     _isSpeaking = false;

//     print('Speaking done');
//     if (cb != null) {
//       cb!();
//     }
//   }

//   // Stop speaking
//   Future<void> stop() async {
//     cb = null;
//     if (speakQueue.isNotEmpty) {
//       speakQueue.clear();
//     }
//     if (_currentText != null) {
//       _currentText = null;
//     }
//     if (!_isSpeaking) return;

//     await _audioPlayer.stop();
//     _isSpeaking = false;
//     _currentText = null;
//     _speakingStateController.add(false);
//   }

//   // Dispose the service
//   void dispose() {
//     _audioPlayer.dispose();
//     _speakingStateController.close();
//   }
// }
