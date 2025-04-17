import 'package:flutter/material.dart';
import './auth-service.dart';
import 'package:flutter/foundation.dart'; // kIsWeb is still here

// *** Add the Conditional Import ***
// Import the stub by default
import '../api/stub_reload_helper.dart'
    // If dart.library.html is available (meaning we are compiling for web),
    // import the web-specific implementation instead.
    if (dart.library.html) '../api/web_reload_helper.dart';

class EmailVerificationPage extends StatefulWidget {
  const EmailVerificationPage({Key? key}) : super(key: key);

  @override
  _EmailVerificationPageState createState() => _EmailVerificationPageState();
}

class _EmailVerificationPageState extends State<EmailVerificationPage> {
  final AuthService _authService = AuthService();
  bool _isResending = false;
  static bool _isInitialized = false;

  // init
  @override
  void initState() {
    super.initState();
    if (!_isInitialized) {
      _isInitialized = true;
      // It might be better UX to not send automatically, but let the user click the button first.
      // Commenting out the automatic resend for now, uncomment if you definitely need it.
      // WidgetsBinding.instance.addPostFrameCallback((_) {
      //   if (mounted) { // Check if the widget is still in the tree
      //      _resendVerificationEmail();
      //   }
      // });
    }
  }

  // Dispose
  @override
  void dispose() {
    // Resetting static variable in dispose might have unintended consequences
    // if multiple instances could theoretically exist or during hot reload.
    // Consider managing this state differently if it causes issues.
    _isInitialized = false;
    super.dispose();
  }

  Future<void> _resendVerificationEmail() async {
    // Prevent multiple simultaneous requests
    if (_isResending) return;

    setState(() {
      _isResending = true;
    });

    try {
      await _authService.sendEmailVerification();
      if (mounted) {
        // Always check mounted before accessing context async-ly
        ScaffoldMessenger.of(context).showSnackBar(
          const SnackBar(
            content: Text(
              '✅ Verification email sent. Please check your inbox.',
            ),
            backgroundColor: Colors.green,
          ),
        );
      }
    } catch (e) {
      if (mounted) {
        // Always check mounted
        ScaffoldMessenger.of(context).showSnackBar(
          SnackBar(
            content: Text('❌ Error: ${e.toString()}'),
            backgroundColor: Colors.red,
          ),
        );
      }
    } finally {
      // Ensure state is updated even if the widget is disposed before finally runs
      if (mounted) {
        setState(() {
          _isResending = false;
        });
      } else {
        _isResending = false; // Update the flag directly if not mounted
      }
    }
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      body: Center(
        child: Padding(
          padding: const EdgeInsets.all(24.0),
          child: Column(
            mainAxisAlignment: MainAxisAlignment.center,
            children: [
              const Icon(
                Icons.mark_email_unread_outlined,
                size: 80,
                color:
                    Colors
                        .blue, // Consider using Theme.of(context).colorScheme.primary
              ),
              const SizedBox(height: 24),
              Text(
                'Verify Your Email',
                style: Theme.of(context).textTheme.headlineMedium?.copyWith(
                  fontWeight: FontWeight.bold,
                ), // Use theme typography
              ),
              const SizedBox(height: 16),
              const Text(
                'Click the button to receive a verification email. Please check your inbox and click the verification link to activate your account.',
                textAlign: TextAlign.center,
                style: TextStyle(
                  fontSize: 16,
                ), // Consider Theme.of(context).textTheme.bodyLarge
              ),
              const SizedBox(height: 8),
              const Text(
                'If you don\'t see it, check your spam folder.',
                textAlign: TextAlign.center,
                style: TextStyle(
                  fontSize: 14,
                  fontStyle: FontStyle.italic,
                  color: Colors.grey,
                ), // Consider Theme.of(context).textTheme.bodySmall
              ),
              const SizedBox(height: 32),
              ElevatedButton.icon(
                // Added icon for better UX
                icon:
                    _isResending
                        ? const SizedBox(
                          width: 20,
                          height: 20,
                          child: CircularProgressIndicator(
                            color:
                                Colors
                                    .white, // Use Theme.of(context).colorScheme.onPrimary
                            strokeWidth: 2,
                          ),
                        )
                        : const Icon(Icons.send),
                // Disable button only when resending, not based on the static flag
                onPressed: _isResending ? null : _resendVerificationEmail,
                label: Text(
                  _isResending ? 'Sending...' : 'Send Verification Email',
                ),
              ),
              const SizedBox(height: 24),
              TextButton(
                onPressed: () async {
                  // It's good practice to show some loading indicator here too
                  ScaffoldMessenger.of(context).showSnackBar(
                    const SnackBar(
                      content: Text('Checking verification status...'),
                    ),
                  );
                  await _authService.forceRefresh();
                  if (mounted) {
                    Navigator.pushNamed(context, "/");
                  }

                  if (kIsWeb) {
                    platformSpecificReload();
                  }
                },
                child: const Text(
                  'I\'ve Verified, Continue',
                ), // Slightly clearer text
              ),
              const SizedBox(height: 16), // Reduced spacing a bit
              TextButton(
                onPressed: () async {
                  await _authService.signOut();
                  if (mounted) {
                    Navigator.pushNamed(context, "/");
                  }
                  if (kIsWeb) {
                    platformSpecificReload();
                  }
                },
                child: Text(
                  'Logout',
                  style: TextStyle(color: Theme.of(context).colorScheme.error),
                ),
              ),
            ],
          ),
        ),
      ),
    );
  }
}
