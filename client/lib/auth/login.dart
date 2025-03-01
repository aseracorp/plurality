import 'package:flutter/material.dart';
import 'package:firebase_auth/firebase_auth.dart';

import './auth-service.dart';
import './login-form.dart';
import './register-form.dart';

// Updated Login Screen
class LoginScreen extends StatefulWidget {
  @override
  _LoginScreenState createState() => _LoginScreenState();
}

class _LoginScreenState extends State<LoginScreen> {
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
      appBar: AppBar(title: Text('Login'), centerTitle: true),
      body: Container(
        padding: EdgeInsets.symmetric(vertical: 20.0, horizontal: 50.0),
        child: LoginForm(
          onSubmit: _handleLogin,
          error: error,
          loading: loading,
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
      appBar: AppBar(title: Text('Register'), centerTitle: true),
      body: Container(
        padding: EdgeInsets.symmetric(vertical: 20.0, horizontal: 50.0),
        child: RegisterForm(
          onSubmit: _handleRegister,
          error: error,
          loading: loading,
        ),
      ),
    );
  }
}
