import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:plurality/api/MCP.dart';
import 'package:plurality/api/skills_service.dart';
import 'package:plurality/api/chat_service.dart';
import './auth/auth-service.dart';
import './auth/account.dart';
import './auth/login.dart';
import './api/storage.dart';
import './utils/index.dart';
import './api/preferences_provider.dart';
import 'chat/index.dart';
import 'dart:io' show Platform;
import 'package:flutter/rendering.dart';

final authStateProvider = StreamProvider<User?>((ref) {
  return AuthService().authStateChanges;
});

void main() async {
  WidgetsFlutterBinding.ensureInitialized();
  await ConversationStorage.init();
  debugPaintSizeEnabled = false;

  checkVersion();

  MCPService().initMCP();
  SkillsService().initSkills();

  runApp(ProviderScope(child: MyApp()));
}

final c = Colors.red;

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
          if (user == null) {
            return LoginScreen();
          }
          ChatService().ensureConnected();
          return ChatScreen(isMobile: isMobile);
        },
        loading: () => LoadingScreen(),
        error: (_, __) => ErrorScreen(),
      ),
      routes: {
        '/login': (context) => LoginScreen(),
        '/account': (context) => SettingsScreen(),
      },
    );
  }
}

class LoadingScreen extends StatelessWidget {
  @override
  Widget build(BuildContext context) {
    return Scaffold(body: Center(child: CircularProgressIndicator()));
  }
}

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
                AuthService().signOut();
              },
              child: Text('Sign Out'),
            ),
          ],
        ),
      ),
    );
  }
}
