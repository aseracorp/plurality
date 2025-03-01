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
          TextFormField(
            decoration: InputDecoration(
              hintText: 'Confirm Password',
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
                (val) => val != password ? 'Passwords do not match' : null,
            obscureText: true,
            onChanged: (val) {
              setState(() => confirmPassword = val);
            },
          ),
          SizedBox(height: 20.0),
          ElevatedButton(
            child: Text('Register', style: TextStyle(color: Colors.white)),
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
            child: Text('Login instead'),
            onPressed: () {
              Navigator.pushNamed(context, '/login');
            },
          ),
        ],
      ),
    );
  }
}
