import 'dart:math';
import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:plurality/api/MCP.dart';
import 'package:plurality/api/chat_service.dart';
import 'package:plurality/auth/email-verify.dart';
import './firebase_options.dart';
import 'package:firebase_auth/firebase_auth.dart';
import 'package:firebase_core/firebase_core.dart';
import './auth/auth-service.dart';
import './auth/account.dart';
import './auth/login.dart';
import './api/storage.dart';
import './api/service.dart';
import './api/stt.dart';
import './utils/index.dart';
import './api/preferences_provider.dart';
import 'chat/index.dart';
import 'package:google_sign_in/google_sign_in.dart';
import 'package:google_sign_in_dartio/google_sign_in_dartio.dart';
import 'dart:io' show Platform;
import 'package:flutter/rendering.dart';
import './api/shared_preferences_service.dart';

// Create an auth state provider

final authStateProvider = StreamProvider<User?>((ref) {
  return FirebaseAuth.instance.authStateChanges();
});

void main() async {
  WidgetsFlutterBinding.ensureInitialized();
  await Firebase.initializeApp(options: DefaultFirebaseOptions.currentPlatform);
  await ConversationStorage.init();
  debugPaintSizeEnabled = false;

  checkVersion();

  var isDesktop =
      !kIsWeb && (Platform.isWindows || Platform.isLinux || Platform.isMacOS);

  if (isDesktop) {
    await GoogleSignInDart.register(
      clientId:
          "986982379072-g44aaifpc7nqilq672frk1p16j0o7a0a.apps.googleusercontent.com",
    );
  }

  MCPService().initMCP();

  runApp(ProviderScope(child: MyApp()));
}

final c = Colors.red;

// Define light and dark themes
final ThemeData lightTheme = ThemeData(
  colorScheme: ColorScheme.fromSeed(seedColor: c),
);

final ThemeData darkTheme = ThemeData(
  colorScheme: ColorScheme.fromSeed(seedColor: c, brightness: Brightness.dark),
);

class MyApp extends ConsumerWidget {
  const MyApp({super.key});

  static bool newVersionAvailable = false;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final isMobile = MediaQuery.of(context).size.width < 820;
    final preferences = ref.watch(preferencesProvider);
    final darkModeValue = preferences.darkMode;
    final isDesktop =
        !kIsWeb && (Platform.isWindows || Platform.isLinux || Platform.isMacOS);
    final defaultZoom = isDesktop ? 1.1 : 1.0;
    final zoomFactor =
        preferences.zoomFactor < 0 ? defaultZoom : preferences.zoomFactor;

    // Determine the theme mode based on user preference
    ThemeMode themeMode;
    switch (darkModeValue) {
      case 0:
        themeMode = ThemeMode.system;
        break;
      case 1:
        themeMode = ThemeMode.light;
        break;
      case 2:
        themeMode = ThemeMode.dark;
        break;
      default:
        themeMode = ThemeMode.system;
    }

    // Watch the auth state
    final authState = ref.watch(authStateProvider);

    return MaterialApp(
      title: 'Plurality',
      theme: lightTheme,
      darkTheme: darkTheme,
      themeMode: themeMode,
      builder: (context, child) {
        return MediaQuery(
          data: MediaQuery.of(context).copyWith(
            textScaler: TextScaler.linear(zoomFactor),
          ),
          child: child!,
        );
      },
      home: authState.when(
        data: (user) {
          // redirect users to the correct screen
          if (user == null) {
            return LoginScreen();
          } else {
            if (!user.emailVerified) {
              return EmailVerificationPage();
            }
            // Connect status stream once (ChatService is a singleton, idempotent check inside)
            ChatService().ensureConnected();
            return ChatScreen(
              isMobile: isMobile,
              // newVersion: true /*checkVersion()*/,
            );
          }
        },
        loading: () => LoadingScreen(),
        error: (_, __) => ErrorScreen(),
      ),
      routes: {
        '/login': (context) => LoginScreen(),
        '/register': (context) => RegisterScreen(),
        '/account': (context) => SettingsScreen(),
        '/verify-email': (context) => EmailVerificationPage(),
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

// Simple error screen
class ErrorScreen extends StatelessWidget {
  @override
  Widget build(BuildContext context) {
    return Scaffold(
      body: Center(
        child: Column(
          mainAxisAlignment: MainAxisAlignment.center,
          children: [
            Icon(Icons.error_outline, color: Colors.red, size: 48),
            SizedBox(height: 16),
            Text('An error occurred. Please try again later.'),
            SizedBox(height: 16),
            ElevatedButton(
              onPressed: () {
                FirebaseAuth.instance.signOut();
              },
              child: Text('Sign Out'),
            ),
          ],
        ),
      ),
    );
  }
}
