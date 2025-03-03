import 'dart:math';

import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import './firebase_options.dart';
import 'package:firebase_auth/firebase_auth.dart';
import 'package:firebase_core/firebase_core.dart';

import './auth/auth-service.dart';
import './auth/login.dart';
import './api/storage.dart';
import './api/service.dart';

import 'chat/index.dart';

void main() async {
  WidgetsFlutterBinding.ensureInitialized();
  await Firebase.initializeApp(options: DefaultFirebaseOptions.currentPlatform);
  await ConversationStorage.init();

  runApp(ProviderScope(child: MyApp()));
}

class MyApp extends StatelessWidget {
  const MyApp({super.key});

  // Color Theme: #ee4654 and #f5c256

  @override
  Widget build(BuildContext context) {
    final isMobile = MediaQuery.of(context).size.width < 820;

    return MaterialApp(
      title: 'Plurality',
      theme: ThemeData(
        colorScheme: ColorScheme.fromSeed(
          // brightness: Brightness.dark,
          seedColor: Color(0xffee4654),
          // surface: Colors.white,
        ),
      ),

      home: ChatScreen(isMobile: isMobile),
      routes: {
        '/login': (context) => LoginScreen(),
        '/register':
            (context) => RegisterScreen(), // Create similar to LoginScreen
        '/home': (context) => ChatScreen(isMobile: isMobile),
      },
    );
  }
}
