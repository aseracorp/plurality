import 'package:flutter/material.dart';
import './firebase_options.dart';
import 'package:firebase_auth/firebase_auth.dart';
import 'package:firebase_core/firebase_core.dart';

import './auth/auth-service.dart';
import './auth/login.dart';

import 'chat/index.dart';

// Step 4: Create a wrapper widget to handle auth state changes
class AuthWrapper extends StatelessWidget {
  @override
  Widget build(BuildContext context) {
    return StreamBuilder<User?>(
      stream: AuthService().authStateChanges,
      builder: (context, snapshot) {
        if (snapshot.connectionState == ConnectionState.active) {
          User? user = snapshot.data;
          if (user == null) {
            return LoginScreen();
          } else {
            return ChatScreen();
          }
        }
        return Scaffold(body: Center(child: CircularProgressIndicator()));
      },
    );
  }
}

void main() async {
  WidgetsFlutterBinding.ensureInitialized();
  await Firebase.initializeApp(options: DefaultFirebaseOptions.currentPlatform);
  runApp(const MyApp());
}

class MyApp extends StatelessWidget {
  const MyApp({super.key});

  // Color Theme: #ee4654 and #f5c256

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      title: 'Plurality',
      theme: ThemeData(
        colorScheme: ColorScheme.fromSeed(
          seedColor: Color(0xffee4654),
          secondary: Color(0xfff5c256),
        ),
      ),
      home: AuthWrapper(),
      routes: {
        '/login': (context) => LoginScreen(),
        '/register':
            (context) => RegisterScreen(), // Create similar to LoginScreen
        '/home': (context) => ChatScreen(),
      },
    );
  }
}
