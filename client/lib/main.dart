import 'dart:math';
import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
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
import './api/preferences_provider.dart';
import 'chat/index.dart';
import 'package:google_sign_in/google_sign_in.dart';
import 'package:google_sign_in_dartio/google_sign_in_dartio.dart';
import 'dart:io' show Platform;
import 'package:flutter/rendering.dart';
import 'package:http/http.dart' as http;
import 'dart:convert';
import './api/shared_preferences_service.dart';
import 'package:flutter/services.dart' show rootBundle;
import '../api/stub_reload_helper.dart'
    // If dart.library.html is available (meaning we are compiling for web),
    // import the web-specific implementation instead.
    if (dart.library.html) '../api/web_reload_helper.dart';

// Create an auth state provider

final authStateProvider = StreamProvider<User?>((ref) {
  return FirebaseAuth.instance.authStateChanges();
});

void checkVersion() async {
  final uri = Uri.parse(
    '/version.json?cacheBust=${DateTime.now().millisecondsSinceEpoch}',
  );
  print('Fetching version info from: $uri');

  final response = await http.get(uri);

  if (response.statusCode == 200) {
    final data = json.decode(response.body);
    final latestVersion = data['version'] as String?;

    // store value in shared preferences

    final String cvJsonString = await rootBundle.loadString('version.json');
    final cvJson = json.decode(cvJsonString);
    final currentVersion = cvJson['version'] as String?;
    print('Current version: $currentVersion');
    print('Latest version: $latestVersion');

    if (currentVersion == "" || currentVersion != latestVersion) {
      print('New version available: $latestVersion');
      if (kIsWeb) {
        platformSpecificReload();
      }
    } else {
      print('No new version available');
    }
  }
}

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

  Widget renderHome(Ref ref) {
    return Text('Hello');
  }

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final isMobile = MediaQuery.of(context).size.width < 820;
    final preferences = ref.watch(preferencesProvider);
    final darkModeValue = preferences.darkMode;

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
      home: authState.when(
        data: (user) {
          // redirect users to the correct screen
          if (user == null) {
            return LoginScreen();
          } else {
            if (!user.emailVerified) {
              return EmailVerificationPage();
            }
            return ChatScreen(isMobile: isMobile);
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
