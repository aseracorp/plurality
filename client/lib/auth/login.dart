import 'package:flutter/material.dart';
import 'package:firebase_auth/firebase_auth.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import './auth-service.dart';
import './login-form.dart';
import './register-form.dart';
import '../api/service.dart';

// Updated Login Screen
class LoginScreen extends ConsumerStatefulWidget {
  @override
  ConsumerState<LoginScreen> createState() => _LoginScreenState();
}

class _LoginScreenState extends ConsumerState<LoginScreen> {
  final AuthService _authService = AuthService();
  String error = '';
  bool loading = false;

  Future<void> _handleLogin(String email, String password) async {
    setState(() => loading = true);
    try {
      await _authService.signInWithEmailPassword(email, password);
      Navigator.pushReplacementNamed(context, '/home');
    } on FirebaseAuthException catch (e) {
      setState(() {
        error = 'Failed to sign in, check your password: ' + (e.message ?? "");
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
                    //assets/logo_512.png
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

// Register Screen
class RegisterScreen extends StatefulWidget {
  @override
  _RegisterScreenState createState() => _RegisterScreenState();
}

class _RegisterScreenState extends State<RegisterScreen> {
  final AuthService _authService = AuthService();
  String error = '';
  bool loading = false;

  Future<void> _handleRegister(String email, String password) async {
    setState(() => loading = true);
    try {
      await _authService.registerWithEmailPassword(email, password);
      Navigator.pushReplacementNamed(context, '/home');
    } on FirebaseAuthException catch (e) {
      setState(() {
        error = 'Failed to register: ' + (e.message ?? "");
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
                    //assets/logo_512.png
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
                    RegisterForm(
                      onSubmit: _handleRegister,
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
