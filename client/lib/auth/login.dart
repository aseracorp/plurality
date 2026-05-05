import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import './auth-service.dart';
import './login-form.dart';

class LoginScreen extends ConsumerStatefulWidget {
  @override
  ConsumerState<LoginScreen> createState() => _LoginScreenState();
}

class _LoginScreenState extends ConsumerState<LoginScreen> {
  final AuthService _authService = AuthService();
  String error = '';
  bool loading = false;

  Future<void> _handleLogin(String username, String password) async {
    setState(() {
      loading = true;
      error = '';
    });
    try {
      await _authService.signInWithEmailPassword(username, password);
      if (mounted) Navigator.pushReplacementNamed(context, '/');
    } catch (e) {
      setState(() {
        error = 'Failed to sign in: ${e.toString()}';
        loading = false;
      });
    }
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      body: Center(
        child: SingleChildScrollView(
          child: Container(
            width: 400,
            padding: EdgeInsets.symmetric(horizontal: 24.0),
            child: Card(
              elevation: 2.0,
              shape: RoundedRectangleBorder(
                borderRadius: BorderRadius.circular(16.0),
              ),
              child: Padding(
                padding: EdgeInsets.all(24.0),
                child: Column(
                  mainAxisSize: MainAxisSize.min,
                  children: [
                    SizedBox(height: 16.0),
                    Image.asset(
                      'assets/logo_512.png',
                      width: 100.0,
                      height: 100.0,
                    ),
                    SizedBox(height: 12.0),
                    Text(
                      'Plurality AI',
                      style: TextStyle(
                        fontSize: 24.0,
                        fontWeight: FontWeight.bold,
                      ),
                    ),
                    SizedBox(height: 24.0),
                    LoginForm(
                      onSubmit: _handleLogin,
                      error: error,
                      loading: loading,
                    ),
                    SizedBox(height: 16.0),
                  ],
                ),
              ),
            ),
          ),
        ),
      ),
    );
  }
}
