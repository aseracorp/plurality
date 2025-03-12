import 'package:flutter/material.dart';
import 'package:firebase_auth/firebase_auth.dart';

import './auth-service.dart';

class RegisterForm extends StatefulWidget {
  final Function(String email, String password) onSubmit;
  final String error;
  final bool loading;

  const RegisterForm({
    Key? key,
    required this.onSubmit,
    this.error = '',
    this.loading = false,
  }) : super(key: key);

  @override
  _RegisterFormState createState() => _RegisterFormState();
}

String? _validatePassword(String? val) {
  if (val!.isEmpty) {
    return 'Enter a password';
  }

  // Password must contain at least one uppercase letter, one lowercase and one number
  if (!RegExp(r'^(?=.*[a-z])(?=.*[A-Z])(?=.*\d).+$').hasMatch(val)) {
    return 'Password must contain at least one uppercase letter, one lowercase and one number';
  }

  if (val.length < 6) {
    return 'Password must be at least 6 characters';
  }

  return null;
}

class _RegisterFormState extends State<RegisterForm> {
  final _formKey = GlobalKey<FormState>();
  String email = '';
  String password = '';
  String confirmPassword = '';

  @override
  Widget build(BuildContext context) {
    return Form(
      key: _formKey,
      child: Column(
        children: <Widget>[
          SizedBox(height: 20.0),
          TextFormField(
            decoration: InputDecoration(
              hintText: 'Email',
              filled: true,
              enabledBorder: OutlineInputBorder(
                borderSide: BorderSide(color: Colors.white, width: 2.0),
              ),
              focusedBorder: OutlineInputBorder(
                borderSide: BorderSide(color: Colors.blue, width: 2.0),
              ),
            ),
            validator: (val) => val!.isEmpty ? 'Enter an email' : null,
            onChanged: (val) {
              setState(() => email = val);
            },
          ),
          SizedBox(height: 20.0),
          TextFormField(
            decoration: InputDecoration(
              hintText: 'Password',
              filled: true,
              enabledBorder: OutlineInputBorder(
                borderSide: BorderSide(color: Colors.white, width: 2.0),
              ),
              focusedBorder: OutlineInputBorder(
                borderSide: BorderSide(color: Colors.blue, width: 2.0),
              ),
            ),
            autovalidateMode: AutovalidateMode.onUserInteraction,
            validator: (val) => _validatePassword(val),
            obscureText: true,
            onChanged: (val) {
              setState(() => password = val);
            },
          ),
          SizedBox(height: 20.0),
          TextFormField(
            decoration: InputDecoration(
              hintText: 'Confirm Password',
              filled: true,
              enabledBorder: OutlineInputBorder(
                borderSide: BorderSide(color: Colors.white, width: 2.0),
              ),
              focusedBorder: OutlineInputBorder(
                borderSide: BorderSide(color: Colors.blue, width: 2.0),
              ),
            ),
            validator:
                (val) => val != password ? 'Passwords do not match' : null,
            obscureText: true,
            onChanged: (val) {
              setState(() => confirmPassword = val);
            },
          ),
          SizedBox(height: 12.0),
          Text(
            widget.error,
            style: TextStyle(color: Colors.red, fontSize: 14.0),
          ),
          SizedBox(height: 20.0),
          ElevatedButton(
            style: ElevatedButton.styleFrom(
              backgroundColor: Theme.of(context).colorScheme.primary,
              foregroundColor: Theme.of(context).colorScheme.onPrimary,
            ),
            child: Text('Register'),
            onPressed:
                widget.loading
                    ? null
                    : () {
                      if (_formKey.currentState!.validate()) {
                        widget.onSubmit(email, password);
                      }
                    },
          ),
          SizedBox(height: 12.0),
          TextButton(
            child: Text('Go to Login'),
            onPressed: () {
              Navigator.pushNamed(context, '/login');
            },
          ),
        ],
      ),
    );
  }
}
