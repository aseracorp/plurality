import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import './auth-service.dart';
import '../api/service.dart';

class LoginForm extends ConsumerStatefulWidget {
  final Function(String username, String password) onSubmit;
  final String error;
  final bool loading;

  const LoginForm({
    Key? key,
    required this.onSubmit,
    this.error = '',
    this.loading = false,
  }) : super(key: key);

  @override
  ConsumerState<LoginForm> createState() => _LoginFormState();
}

class _LoginFormState extends ConsumerState<LoginForm> {
  final AuthService _authService = AuthService();
  final _formKey = GlobalKey<FormState>();
  String username = '';
  String password = '';
  AuthMethods? _methods;
  bool _openidLoading = false;
  String? _openidError;

  @override
  void initState() {
    super.initState();
    _loadMethods();
  }

  Future<void> _loadMethods() async {
    final m = await _authService.getAuthMethods();
    if (mounted) setState(() => _methods = m);
  }

  void _resetConv() {
    final conversationsNotifier = ref.read(conversationsProvider.notifier);
    conversationsNotifier.deleteAllConversations();
  }

  Future<void> _handleOpenID() async {
    final methods = _methods;
    if (methods == null) return;
    setState(() {
      _openidLoading = true;
      _openidError = null;
    });
    try {
      _resetConv();
      await _authService.signInWithOpenID(methods);
      if (mounted) Navigator.of(context).pushReplacementNamed('/');
    } catch (e) {
      if (mounted) setState(() => _openidError = e.toString());
    } finally {
      if (mounted) setState(() => _openidLoading = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final showOpenId = _methods?.openidReady == true;
    final disabled = widget.loading || _openidLoading;
    return Form(
      key: _formKey,
      child: Column(
        children: <Widget>[
          SizedBox(height: 20.0),
          TextFormField(
            decoration: InputDecoration(
              hintText: 'Username',
              filled: true,
              enabledBorder: OutlineInputBorder(
                borderSide: BorderSide(color: Colors.white, width: 2.0),
              ),
              focusedBorder: OutlineInputBorder(
                borderSide: BorderSide(color: Colors.blue, width: 2.0),
              ),
            ),
            validator: (val) =>
                val == null || val.isEmpty ? 'Enter a username' : null,
            onChanged: (val) => setState(() => username = val),
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
            validator: (val) =>
                val == null || val.length < 4 ? 'Enter a password (4+ chars)' : null,
            obscureText: true,
            onChanged: (val) => setState(() => password = val),
          ),
          SizedBox(height: 12.0),
          if (widget.error.isNotEmpty)
            Padding(
              padding: const EdgeInsets.only(bottom: 8.0),
              child: Text(
                widget.error,
                style: TextStyle(color: Colors.red, fontSize: 14.0),
              ),
            ),
          ElevatedButton(
            style: ElevatedButton.styleFrom(
              backgroundColor: Theme.of(context).colorScheme.primary,
              foregroundColor: Theme.of(context).colorScheme.onPrimary,
            ),
            child: Text('Sign In'),
            onPressed: disabled
                ? null
                : () {
                    if (_formKey.currentState!.validate()) {
                      _resetConv();
                      widget.onSubmit(username, password);
                    }
                  },
          ),
          if (showOpenId) ...[
            SizedBox(height: 20.0),
            ElevatedButton(
              style: ElevatedButton.styleFrom(
                backgroundColor: Theme.of(context).colorScheme.secondary,
                foregroundColor: Theme.of(context).colorScheme.onSecondary,
              ),
              child: _openidLoading
                  ? const SizedBox(
                      height: 16,
                      width: 16,
                      child: CircularProgressIndicator(strokeWidth: 2),
                    )
                  : Text('Sign In With OpenID'),
              onPressed: disabled ? null : _handleOpenID,
            ),
            if (_openidError != null)
              Padding(
                padding: const EdgeInsets.only(top: 8.0),
                child: Text(
                  _openidError!,
                  style: const TextStyle(color: Colors.red, fontSize: 14.0),
                ),
              ),
          ],
        ],
      ),
    );
  }
}
