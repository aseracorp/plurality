import 'dart:io';

import 'package:flutter_tts/flutter_tts.dart';
import 'dart:async';
import 'package:flutter/foundation.dart';

class TTSService {
  // Singleton pattern
  static final TTSService _instance = TTSService._internal();
  factory TTSService() => _instance;
  TTSService._internal();

  final FlutterTts _flutterTts = FlutterTts();
  bool _isSpeaking = false;

  // Stream controller to notify listeners about speaking state changes
  final _speakingStateController = StreamController<bool>.broadcast();
  Stream<bool> get speakingState => _speakingStateController.stream;

  // Current text being spoken
  String? _currentText;
  String? get currentText => _currentText;

  // Initialize the TTS service
  Future<void> initialize() async {
    _flutterTts.setCompletionHandler(() {
      _isSpeaking = false;
      _currentText = null;
      _speakingStateController.add(false);
    });
  }

  // Check if TTS is currently speaking
  bool get isSpeaking => _isSpeaking;

  // Extract plain text from markdown
  String _extractPlainText(String markdownText) {
    return markdownText
        .replaceAll(RegExp(r'```[\s\S]*?```'), '') // Remove code blocks
        .replaceAll(RegExp(r'`[^`]*`'), '') // Remove inline code
        .replaceAll(RegExp(r'!\[.*?\]\(.*?\)'), '') // Remove images
        .replaceAll(
          RegExp(r'\[([^\]]*)\]\([^)]*\)'),
          r'$1',
        ) // Replace links with just their text
        .replaceAll(RegExp(r'#+\s+'), '') // Remove heading markers
        .replaceAll(
          RegExp(r'\*\*|\*|__|\|'),
          '',
        ) // Remove bold, italic, table markers
        .replaceAll(RegExp(r'>\s+'), '') // Remove blockquote markers
        .replaceAll(RegExp(r'={2,}'), '=') // Remove blockquote markers
        .replaceAll(RegExp(r'- |\d+\. '), '') // Remove list markers
        .trim();
  }

  // Speak text
  Future<void> speak(String markdownText) async {
    if (markdownText.isEmpty) return;

    if (!kIsWeb && Platform.isAndroid) await _flutterTts.setSpeechRate(1.2);

    // Stop any current speech
    if (_isSpeaking) {
      await stop();
    }

    String plainText = _extractPlainText(markdownText);

    _isSpeaking = true;
    _currentText = markdownText;
    _speakingStateController.add(true);

    await _flutterTts.speak(plainText);
  }

  // Stop speaking
  Future<void> stop() async {
    if (!_isSpeaking) return;

    await _flutterTts.stop();
    _isSpeaking = false;
    _currentText = null;
    _speakingStateController.add(false);
  }

  // Dispose the service
  void dispose() {
    _flutterTts.stop();
    _speakingStateController.close();
  }
}
