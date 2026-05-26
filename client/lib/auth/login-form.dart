import 'package:flutter/foundation.dart' show kIsWeb;
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
  final TextEditingController _serverUrlController = TextEditingController();
  String username = '';
  String password = '';
  String serverUrl = '';
  bool _serverUrlValid = false;
  AuthMethods? _methods;
  bool _methodsConfirmed = false;
  bool _methodsLoading = false;
  String? _methodsError;
  bool _openidLoading = false;
  String? _openidError;

  @override
  void initState() {
    super.initState();
    if (!kIsWeb) {
      final saved = AuthService.nativeServerUrl;
      _serverUrlController.text = saved;
      serverUrl = saved;
      _serverUrlValid = _validateServerUrl(saved) == null;
    } else {
      // Web talks to its own origin — there's no URL step, so go straight to
      // querying the server for its supported methods.
      _serverUrlValid = true;
      _loadMethods();
    }
  }

  @override
  void dispose() {
    _serverUrlController.dispose();
    super.dispose();
  }

  String? _validateServerUrl(String? val) {
    if (val == null || val.trim().isEmpty) return 'Enter a server URL';
    final uri = Uri.tryParse(val.trim());
    if (uri == null) return 'Invalid URL';
    if (uri.scheme != 'http' && uri.scheme != 'https') {
      return 'URL must start with http:// or https://';
    }
    if (uri.host.isEmpty) return 'URL must include a host';
    return null;
  }

  /// Fetch the server's supported login methods. On native this advances the
  /// form to step 2; a null result (unreachable/error) keeps us on step 1 with
  /// an error message.
  Future<void> _loadMethods() async {
    setState(() {
      _methodsLoading = true;
      _methodsError = null;
    });
    final m = await _authService.getAuthMethods();
    if (!mounted) return;
    setState(() {
      _methodsLoading = false;
      if (m == null) {
        _methodsError = 'Could not reach server';
        _methodsConfirmed = false;
      } else {
        _methods = m;
        _methodsConfirmed = true;
      }
    });
  }

  void _onServerUrlChanged(String val) {
    setState(() {
      serverUrl = val;
      _serverUrlValid = _validateServerUrl(val) == null;
    });
  }

  Future<void> _handleContinue() async {
    if (_validateServerUrl(serverUrl) != null) return;
    await AuthService.setServerUrl(serverUrl);
    await _loadMethods();
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
      if (!kIsWeb) {
        await AuthService.setServerUrl(serverUrl);
      }
      _resetConv();
      await _authService.signInWithOpenID(methods);
      if (mounted) Navigator.of(context).pushReplacementNamed('/');
    } catch (e) {
      if (mounted) setState(() => _openidError = e.toString());
    } finally {
      if (mounted) setState(() => _openidLoading = false);
    }
  }

  InputDecoration _fieldDecoration(String hint) => InputDecoration(
        hintText: hint,
        filled: true,
        enabledBorder: OutlineInputBorder(
          borderSide: BorderSide(color: Colors.white, width: 2.0),
        ),
        focusedBorder: OutlineInputBorder(
          borderSide: BorderSide(color: Colors.blue, width: 2.0),
        ),
      );

  @override
  Widget build(BuildContext context) {
    // Native shows a URL step first; web talks to its own origin and jumps
    // straight to the auth controls once methods have loaded.
    final showUrlStep = !kIsWeb && !_methodsConfirmed;
    return Form(
      key: _formKey,
      child: Column(
        children: <Widget>[
          SizedBox(height: 20.0),
          if (showUrlStep) ..._buildUrlStep(context) else ..._buildAuthStep(context),
        ],
      ),
    );
  }

  // Step 1 (native): enter the server URL, then fetch its supported methods.
  List<Widget> _buildUrlStep(BuildContext context) {
    return [
      TextFormField(
        controller: _serverUrlController,
        decoration: _fieldDecoration('Server URL (https://...)'),
        keyboardType: TextInputType.url,
        autocorrect: false,
        validator: _validateServerUrl,
        onChanged: _onServerUrlChanged,
      ),
      SizedBox(height: 12.0),
      if (_methodsError != null)
        Padding(
          padding: const EdgeInsets.only(bottom: 8.0),
          child: Text(
            _methodsError!,
            style: const TextStyle(color: Colors.red, fontSize: 14.0),
          ),
        ),
      ElevatedButton(
        style: ElevatedButton.styleFrom(
          backgroundColor: Theme.of(context).colorScheme.primary,
          foregroundColor: Theme.of(context).colorScheme.onPrimary,
        ),
        onPressed: (!_serverUrlValid || _methodsLoading) ? null : _handleContinue,
        child: _methodsLoading
            ? const SizedBox(
                height: 16,
                width: 16,
                child: CircularProgressIndicator(strokeWidth: 2),
              )
            : Text('Continue'),
      ),
    ];
  }

  // Step 2: render whichever login methods the server reported.
  List<Widget> _buildAuthStep(BuildContext context) {
    final methods = _methods;
    final showLocal = methods?.local == true;
    final showOpenId = methods?.openidReady == true;
    final disabled = widget.loading || _openidLoading;

    return [
      if (!kIsWeb)
        Align(
          alignment: Alignment.centerLeft,
          child: TextButton.icon(
            icon: const Icon(Icons.arrow_back, size: 16),
            label: Text(serverUrl),
            onPressed: disabled
                ? null
                : () => setState(() {
                      _methodsConfirmed = false;
                      _openidError = null;
                    }),
          ),
        ),
      if (!showLocal && !showOpenId)
        Padding(
          padding: const EdgeInsets.symmetric(vertical: 12.0),
          child: Text(
            'This server has no configured login methods.',
            style: const TextStyle(color: Colors.red, fontSize: 14.0),
            textAlign: TextAlign.center,
          ),
        ),
      if (showLocal) ...[
        TextFormField(
          decoration: _fieldDecoration('Username'),
          validator: (val) =>
              val == null || val.isEmpty ? 'Enter a username' : null,
          onChanged: (val) => setState(() => username = val),
        ),
        SizedBox(height: 20.0),
        TextFormField(
          decoration: _fieldDecoration('Password'),
          validator: (val) => val == null || val.length < 4
              ? 'Enter a password (4+ chars)'
              : null,
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
              : () async {
                  if (_formKey.currentState!.validate()) {
                    if (!kIsWeb) {
                      await AuthService.setServerUrl(serverUrl);
                    }
                    _resetConv();
                    widget.onSubmit(username, password);
                  }
                },
        ),
      ],
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
    ];
  }
}
