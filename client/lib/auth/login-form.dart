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

  // Method to handle password reset
  void _resetPassword(BuildContext context) async {
    if (email.isEmpty) {
      ScaffoldMessenger.of(context).showSnackBar(
        SnackBar(
          content: Text('Please enter your email first'),
          backgroundColor: Colors.red,
        ),
      );
      return;
    }

    try {
      await FirebaseAuth.instance.sendPasswordResetEmail(email: email);
      ScaffoldMessenger.of(context).showSnackBar(
        SnackBar(
          content: Text('Password reset email sent. Check your inbox.'),
          backgroundColor: Colors.green,
        ),
      );
    } catch (e) {
      ScaffoldMessenger.of(context).showSnackBar(
        SnackBar(
          content: Text('Error: ${e.toString()}'),
          backgroundColor: Colors.red,
        ),
      );
    }
  }

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
          SizedBox(height: 12.0),
          Text(
            widget.error,
            style: TextStyle(color: Colors.red, fontSize: 14.0),
          ),
          Row(
            mainAxisAlignment: MainAxisAlignment.center,
            children: [
              TextButton(
                child: Text('Register'),
                onPressed: () {
                  Navigator.pushNamed(context, '/register');
                },
              ),
              TextButton(
                child: Text('Forgot Password'),
                onPressed: () => _resetPassword(context),
              ),
            ],
          ),
          SizedBox(height: 20.0),
          ElevatedButton(
            style: ElevatedButton.styleFrom(
              backgroundColor: Theme.of(context).colorScheme.primary,
              foregroundColor: Theme.of(context).colorScheme.onPrimary,
            ),
            child: Text('Sign In'),
            onPressed:
                widget.loading
                    ? null
                    : () {
                      if (_formKey.currentState!.validate()) {
                        widget.onSubmit(email, password);
                      }
                    },
          ),
        ],
      ),
    );
  }
}
