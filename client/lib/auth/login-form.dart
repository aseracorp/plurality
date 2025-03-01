import 'package:flutter/material.dart';
import 'package:firebase_auth/firebase_auth.dart';

import './auth-service.dart';

class LoginForm extends StatefulWidget {
  final Function(String email, String password) onSubmit;
  final String error;
  final bool loading;

  const LoginForm({
    Key? key,
    required this.onSubmit,
    this.error = '',
    this.loading = false,
  }) : super(key: key);

  @override
  _LoginFormState createState() => _LoginFormState();
}

class _LoginFormState extends State<LoginForm> {
  final _formKey = GlobalKey<FormState>();
  String email = '';
  String password = '';

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
              fillColor: Colors.white,
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
              fillColor: Colors.white,
              enabledBorder: OutlineInputBorder(
                borderSide: BorderSide(color: Colors.white, width: 2.0),
              ),
              focusedBorder: OutlineInputBorder(
                borderSide: BorderSide(color: Colors.blue, width: 2.0),
              ),
            ),
            validator:
                (val) =>
                    val!.length < 6 ? 'Enter a password 6+ chars long' : null,
            obscureText: true,
            onChanged: (val) {
              setState(() => password = val);
            },
          ),
          SizedBox(height: 20.0),
          ElevatedButton(
            child: Text('Sign In', style: TextStyle(color: Colors.white)),
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
          Text(
            widget.error,
            style: TextStyle(color: Colors.red, fontSize: 14.0),
          ),
          TextButton(
            child: Text('Register instead'),
            onPressed: () {
              Navigator.pushNamed(context, '/register');
            },
          ),
        ],
      ),
    );
  }
}
