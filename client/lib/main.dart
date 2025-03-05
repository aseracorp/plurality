import 'dart:math';
import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import './firebase_options.dart';
import 'package:firebase_auth/firebase_auth.dart';
import 'package:firebase_core/firebase_core.dart';
import './auth/auth-service.dart';
import './auth/account.dart';
import './auth/login.dart';
import './api/storage.dart';
import './api/service.dart';
import 'chat/index.dart';

// Flag to track if we've completed the initial auth check
bool _initialAuthCheckComplete = false;

void main() async {
  WidgetsFlutterBinding.ensureInitialized();
  await Firebase.initializeApp(options: DefaultFirebaseOptions.currentPlatform);
  await ConversationStorage.init();

  // if (!kIsWeb && Platform.isWindows) {
  //   await GoogleSignInDart.register(
  //     clientId:
  //         '406099696497-g5o9l0blii9970bgmfcfv14pioj90djd.apps.googleusercontent.com',
  //   );
  // }

  // Wait for Firebase Auth to restore session from persistence
  // This is critical - we need to know auth state before showing ANY UI

  _initialAuthCheckComplete = true;

  runApp(ProviderScope(child: MyApp()));
}

class MyApp extends ConsumerWidget {
  const MyApp({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final isMobile = MediaQuery.of(context).size.width < 820;
    return MaterialApp(
      title: 'Plurality',
      theme: ThemeData(
        colorScheme: ColorScheme.fromSeed(seedColor: Color(0xffee4654)),
      ),
      // No longer passing user ID here - the AuthGate component handles it
      home: AuthGate(isMobile: isMobile),
      routes: {
        '/login': (context) => LoginScreen(),
        '/register': (context) => RegisterScreen(),
        '/home': (context) => ChatScreen(isMobile: isMobile),
        '/account': (context) => SettingsScreen(),
      },
    );
  }
}

// A simple loading screen
class LoadingScreen extends StatelessWidget {
  @override
  Widget build(BuildContext context) {
    return Scaffold(body: Center(child: CircularProgressIndicator()));
  }
}

// Use this for any routes that need auth protection after initial load
// Updated AuthGate with proper initialization check
class AuthGate extends StatefulWidget {
  final bool isMobile;

  const AuthGate({required this.isMobile, Key? key}) : super(key: key);

  @override
  _AuthGateState createState() => _AuthGateState();
}

class _AuthGateState extends State<AuthGate> {
  bool _isInitialized = false;
  User? _currentUser;

  @override
  void initState() {
    super.initState();
    _checkAuthState();
  }

  Future<void> _checkAuthState() async {
    // Listen for auth state changes
    FirebaseAuth.instance.authStateChanges().listen((User? user) {
      if (mounted) {
        setState(() {
          _currentUser = user;
          _isInitialized = true;
        });
      }
    });
  }

  @override
  Widget build(BuildContext context) {
    // Show loading screen while Firebase Auth is initializing
    if (!_isInitialized) {
      return LoadingScreen();
    }

    // Once initialized, show the appropriate screen based on auth state
    if (_currentUser != null) {
      return ChatScreen(isMobile: widget.isMobile);
    } else {
      return LoginScreen();
    }
  }
}
