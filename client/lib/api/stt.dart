import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'package:flutter/material.dart';
import 'package:flutter/foundation.dart';
import 'package:pcmtowave/pcmtowave.dart';
import 'package:avatar_glow/avatar_glow.dart';
import 'package:pcmtowave/convertToWav.dart';
import 'package:record/record.dart';
import 'package:path_provider/path_provider.dart';
import '../utils/types.dart';
import './api.dart';
import './tts.dart';

class SpeechRecognitionService {
  // Singleton pattern
  static final SpeechRecognitionService _instance =
      SpeechRecognitionService._internal();
  factory SpeechRecognitionService() => _instance;
  SpeechRecognitionService._internal();

  final _record = AudioRecorder();
  bool _isRecording = false;
  String recognizedText = '';
  BuildContext? _modalContext;
  final ApiService _apiService = ApiService();

  // Stream controllers to notify listeners
  final _recordingStateController = StreamController<bool>.broadcast();
  final _textRecognizedController = StreamController<String>.broadcast();

  // New stream controller for amplitude
  final _amplitudeController = StreamController<double>.broadcast();
  final _listeningController = StreamController<bool>.broadcast();

  // Timer for amplitude updates
  Timer? _amplitudeTimer;

  // Public streams
  Stream<bool> get recordingState => _recordingStateController.stream;

  // New amplitude stream
  Stream<double> get amplitudeStream => _amplitudeController.stream;

  // New amplitude stream
  Stream<bool> get listeningStream => _listeningController.stream;

  // Check if recording is currently active
  bool get isRecording => _isRecording;

  // Get the current recognized text
  String get currentText => recognizedText;

  final List<double> _amplitudeHistory = [];
  double _silenceThreshold = -35.0; // Default threshold in dB
  bool _thresholdCalculated = false;

  int _silenceCounter = 0;

  var voiceStreamBytes = <int>[];

  bool isCall = false;

  // Method to calculate silence threshold based on amplitude history
  void _calculateSilenceThreshold() {
    if (_amplitudeHistory.length < 20) {
      // Not enough data points yet
      return;
    }

    // Sort the amplitude values to find percentiles
    List<double> sortedAmplitudes = List.from(_amplitudeHistory)..sort();

    // Calculate the 90th percentile
    int index = (sortedAmplitudes.length * 0.9).floor();
    _silenceThreshold = sortedAmplitudes[index] - 10.0; // Adjusted for silence

    // print(
    //   'Calculated silence threshold: $_silenceThreshold dB (90th percentile)',
    // );
    _thresholdCalculated = true;
  }

  // Modified _startAmplitudeMonitoring method
  void _startAmplitudeMonitoring({bool autoStop = false}) {
    // Reset amplitude history when starting a new recording
    _amplitudeHistory.clear();
    _thresholdCalculated = false;

    // Cancel any existing timer
    _amplitudeTimer?.cancel();

    // Create a new timer that fires every 100ms
    _amplitudeTimer = Timer.periodic(const Duration(milliseconds: 100), (
      timer,
    ) async {
      if (_isRecording) {
        try {
          // Get the current amplitude
          final amplitude = await _record.getAmplitude();
          final double level = amplitude.current ?? 0.0;

          // Add to amplitude history (keep last 300 values)
          _amplitudeHistory.add(level);
          if (_amplitudeHistory.length > 300) {
            _amplitudeHistory.removeAt(0);
          }

          // Calculate threshold after collecting enough samples
          if (_amplitudeHistory.length >= 20) {
            _calculateSilenceThreshold();
          }

          // Add the amplitude to the stream
          _amplitudeController.add(level);

          // Check for silence (optional)
          if (_thresholdCalculated) {
            bool isSilent = level <= _silenceThreshold;
            if (isSilent && autoStop) {
              _silenceCounter++;
              if (_silenceCounter > 10) {
                // If silent for 20 intervals, stop recording
                _silenceCounter = 0; // Reset counter
                await stopRecording(); // Stop recording
                print('Silence detected, stopping recording...');
              }
            } else {
              _silenceCounter = 0; // Reset counter if not silent
            }
          }
        } catch (e) {
          print('Error getting amplitude: $e');
        }
      } else {
        // Stop the timer if not recording
        timer.cancel();
      }
    });
  }

  // Add a getter for the silence threshold
  double get silenceThreshold => _silenceThreshold;

  // Add a method to check if current amplitude is silence
  bool isCurrentlySilent(double amplitude) {
    return _thresholdCalculated && amplitude <= _silenceThreshold;
  }

  // Start recording and show modal
  Future<void> startRecording(
    BuildContext context, {
    bool autoStop = false,
    bool call = false,
  }) async {
    // Check permissions
    if (!await _record.hasPermission()) {
      return;
    }

    _listeningController.add(true);

    isCall = call;

    // Start recording
    var voiceStream = await _record.startStream(
      const RecordConfig(
        encoder: AudioEncoder.pcm16bits,
        bitRate: 128000,
        numChannels: 1,
        sampleRate: 8000,
      ),
    );

    voiceStream.listen(
      (data) => voiceStreamBytes.addAll(data),
      onError: (error) {
        print('Error in voice stream: $error');
      },
      onDone: () {
        print('Voice stream done');
      },
    );

    // voiceStream!.listen(
    //   (data) => voiceStreamBytes.addAll(data),
    //   onError: (error) {
    //     print('Error in voice stream: $error');
    //   },
    //   onDone: () {
    //     print('Voice stream done');
    //   },
    // );

    _isRecording = true;
    _recordingStateController.add(true);
    recognizedText = '';

    // Start amplitude monitoring
    _startAmplitudeMonitoring(autoStop: autoStop);

    // Show modal
    _showRecordingModal(context);
  }

  // Stop recording and transcribe
  Future<String> stopRecording() async {
    if (!_isRecording) return '';

    // Stop amplitude monitoring
    _amplitudeTimer?.cancel();

    // Stop recording
    await _record.stop();

    try {
      // Convert to base64
      // final base64Audio = base64Encode(bytes);

      // Call the transcribe API
      // wait 500ms before calling the API

      await Future.delayed(const Duration(milliseconds: 100));

      print(voiceStreamBytes.length);

      recognizedText = await _apiService.transcribeAudio(
        Uint8List.fromList(voiceStreamBytes.toList()),
      );
      voiceStreamBytes = <int>[]; // Clear the stream bytes

      print('[Internal] Transcribed text: $recognizedText');

      _textRecognizedController.add(recognizedText);

      return recognizedText;
    } catch (e) {
      _isRecording = false;
      _listeningController.add(false);
      _recordingStateController.add(false);
      print('Error transcribing audio: $e');
      return '';
    } finally {
      _isRecording = false;
      _recordingStateController.add(false);
      _listeningController.add(false);
      if (!isCall) _dismissModal();
    }
  }

  // Cancel recording
  Future<void> cancelRecording() async {
    // Stop amplitude monitoring
    _amplitudeTimer?.cancel();

    voiceStreamBytes = <int>[]; // Clear the stream bytes

    isCall = false;

    await _record.stop();
    _isRecording = false;
    _listeningController.add(false);
    _recordingStateController.add(false);
    recognizedText = '';

    TTSService().stop();

    _dismissModal();
  }

  // Show the recording modal
  void _showRecordingModal(BuildContext context) {
    if (_modalContext == null) {
      showDialog(
        context: context,
        barrierDismissible: false,
        builder: (BuildContext modalContext) {
          _modalContext = modalContext;
          return _RecordingModal(
            onCancel: () => cancelRecording(),
            onDone: () => stopRecording(),
            onInterrupt: () {
              TTSService().stop();
              startRecording(context, autoStop: true, call: isCall);
            },
            textStream: _textRecognizedController.stream,
            amplitudeStream:
                _amplitudeController.stream, // Pass the amplitude stream
            listeningStream: _listeningController.stream,
            isCall: isCall,
          );
        },
      );
    }
  }

  // Dismiss the modal if it's showing
  void _dismissModal() {
    if (_modalContext != null) {
      Navigator.of(_modalContext!).pop();
      _modalContext = null;
    }
  }

  // Dispose the service
  void dispose() {
    _record.dispose();
    _recordingStateController.close();
    _textRecognizedController.close();
    _amplitudeController.close(); // Close the amplitude stream controller
    _amplitudeTimer?.cancel(); // Cancel the timer
  }
}

// Modal widget to show while recording
class _RecordingModal extends StatefulWidget {
  final VoidCallback onCancel;
  final VoidCallback onDone;
  final VoidCallback onInterrupt;
  final Stream<String> textStream;
  final Stream<double> amplitudeStream; // New parameter for amplitude stream
  final Stream<bool> listeningStream;
  final bool isCall;

  const _RecordingModal({
    Key? key,
    required this.onCancel,
    required this.onDone,
    required this.onInterrupt,
    required this.textStream,
    required this.amplitudeStream, // Add this parameter
    required this.listeningStream,
    required this.isCall,
  }) : super(key: key);

  @override
  _RecordingModalState createState() => _RecordingModalState();
}

class _RecordingModalState extends State<_RecordingModal>
    with SingleTickerProviderStateMixin {
  late AnimationController _animationController;
  late Animation<double> _opacityAnimation;
  String recognizedText = '';
  double currentAmplitude = 0.0; // Track current amplitude
  bool isSilent = false;
  double silenceThreshold = -35.0; // Track silence threshold
  bool isListening = true;

  @override
  void initState() {
    super.initState();

    // Set up animation for the "Listening..." text
    _animationController = AnimationController(
      vsync: this,
      duration: const Duration(milliseconds: 1000),
    )..repeat(reverse: true);

    _opacityAnimation = Tween<double>(
      begin: 0.6,
      end: 1.0,
    ).animate(_animationController);

    // Listen to recognized text updates
    widget.textStream.listen((text) {
      if (mounted) {
        setState(() {
          recognizedText = text;
        });
      }
    });

    // Listen to amplitude updates
    widget.amplitudeStream.listen((amplitude) {
      if (mounted) {
        final speechService = SpeechRecognitionService();
        setState(() {
          currentAmplitude = amplitude;
          isSilent = speechService.isCurrentlySilent(amplitude);
          silenceThreshold = speechService.silenceThreshold;
        });
      }
    });

    // Listen to listening state updates
    widget.listeningStream.listen((_isListening) {
      if (mounted) {
        setState(() {
          isListening = _isListening;
        });
      }
    });
  }

  @override
  void dispose() {
    _animationController.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    // Calculate a normalized amplitude value between 0 and 1
    // The getAmplitude() method typically returns values in decibels
    // We'll map it to a reasonable range for visualization
    // print(currentAmplitude);

    double normalizedAmplitude =
        (currentAmplitude - silenceThreshold) / (-silenceThreshold);
    normalizedAmplitude = normalizedAmplitude.clamp(0.01, 1.0);

    return AlertDialog(
      shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(16)),
      content: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
          const SizedBox(height: 16),
          // Audio level visualizer
          if (isListening)
            AvatarGlow(
              glowColor: Theme.of(context).primaryColor,
              duration: const Duration(milliseconds: 2000),
              repeat: true,
              child: AnimatedContainer(
                duration: const Duration(milliseconds: 50),
                width: 80,
                height: 75,
                decoration: BoxDecoration(
                  shape: BoxShape.circle,
                  color: Theme.of(context).primaryColor.withOpacity(0.2),
                ),
                child: Center(
                  child: Icon(
                    Icons.mic,
                    color: Theme.of(context).primaryColor,
                    size: 40,
                  ),
                ),
              ),
            ),
          if (!isListening)
            GestureDetector(
              onTap: () {
                widget.onInterrupt();
              },
              child: Container(
                width: 80,
                height: 75,
                decoration: BoxDecoration(
                  shape: BoxShape.circle,
                  color: Colors.red.withOpacity(0.2),
                ),
                child: Center(
                  child: Icon(Icons.mic_off, color: Colors.white, size: 40),
                ),
              ),
            ),

          const SizedBox(height: 16),
          /*Container(
            padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 4),
            decoration: BoxDecoration(
              color: isSilent ? Colors.grey : Theme.of(context).primaryColor,
              borderRadius: BorderRadius.circular(12),
            ),
            child: Text(
              isSilent ? "Silent" : "Speaking",
              style: TextStyle(color: Colors.white, fontSize: 12),
            ),
          ),*/
          // Audio level indicator bar
          Container(
            margin: const EdgeInsets.symmetric(vertical: 10),
            width: double.infinity,
            height: 10,
            decoration: BoxDecoration(
              color: Colors.grey.withOpacity(0.2),
              borderRadius: BorderRadius.circular(5),
            ),
            child: FractionallySizedBox(
              alignment: Alignment.centerLeft,
              widthFactor: normalizedAmplitude,
              child: Container(
                decoration: BoxDecoration(
                  color: Theme.of(context).primaryColor,
                  borderRadius: BorderRadius.circular(5),
                ),
              ),
            ),
          ),

          const SizedBox(height: 16),
          FadeTransition(
            opacity: _opacityAnimation,
            child: Text(
              isListening ? "Listening..." : "Speaking...",
              style: TextStyle(fontSize: 22, fontWeight: FontWeight.bold),
            ),
          ),
          if (!isListening)
            FadeTransition(
              opacity: _opacityAnimation,
              child: Text(
                "Unmute yourself to interrupt",
                style: TextStyle(fontSize: 14),
              ),
            ),
          const SizedBox(height: 20),
          if (false && recognizedText.isNotEmpty)
            Container(
              padding: const EdgeInsets.all(12),
              decoration: BoxDecoration(
                color: Colors.grey.withOpacity(0.1),
                borderRadius: BorderRadius.circular(8),
              ),
              constraints: const BoxConstraints(maxHeight: 100),
              width: double.infinity,
              child: SingleChildScrollView(
                child: Text(
                  recognizedText,
                  style: const TextStyle(fontSize: 16),
                ),
              ),
            ),
          const SizedBox(height: 24),
          Row(
            mainAxisAlignment: MainAxisAlignment.spaceEvenly,
            children: [
              if (!widget.isCall)
                ElevatedButton.icon(
                  onPressed: widget.onCancel,
                  icon: const Icon(Icons.close, color: Colors.white),
                  label: const Text("Cancel"),
                  style: ElevatedButton.styleFrom(
                    backgroundColor: Colors.grey,
                    foregroundColor: Colors.white,
                    padding: const EdgeInsets.symmetric(
                      horizontal: 16,
                      vertical: 10,
                    ),
                  ),
                ),

              if (!widget.isCall)
                ElevatedButton.icon(
                  onPressed: widget.onDone,
                  icon: const Icon(Icons.check, color: Colors.white),
                  label: const Text("Done"),
                  style: ElevatedButton.styleFrom(
                    backgroundColor: Theme.of(context).primaryColor,
                    foregroundColor: Colors.white,
                    padding: const EdgeInsets.symmetric(
                      horizontal: 16,
                      vertical: 10,
                    ),
                  ),
                ),

              if (widget.isCall)
                ElevatedButton.icon(
                  onPressed: widget.onCancel,
                  icon: const Icon(Icons.phone, color: Colors.white),
                  label: const Text("End Call"),
                  style: ElevatedButton.styleFrom(
                    backgroundColor: Colors.red,
                    foregroundColor: Colors.white,
                    padding: const EdgeInsets.symmetric(
                      horizontal: 16,
                      vertical: 10,
                    ),
                  ),
                ),
            ],
          ),
        ],
      ),
    );
  }
}
