import 'package:flutter/material.dart';
import 'package:firebase_auth/firebase_auth.dart';
import 'package:firebase_core/firebase_core.dart';
import 'package:google_sign_in/google_sign_in.dart';
import 'package:google_sign_in_dartio/google_sign_in_dartio.dart';
import 'package:flutter/foundation.dart';

class AuthService {
  final FirebaseAuth _auth = FirebaseAuth.instance;

  // Auth state changes stream
  Stream<User?> get authStateChanges => _auth.authStateChanges();

  Future<UserCredential> signInWithGoogle() async {
    // Trigger the authentication flow
    final GoogleSignInAccount? googleUser =
        await GoogleSignIn(
          clientId:
              kIsWeb
                  ? "986982379072-rnlbo109kg5db0pe3f9nt866l76drsm4.apps.googleusercontent.com"
                  : null,
        ).signIn();

    // Obtain the auth details from the request
    final GoogleSignInAuthentication? googleAuth =
        await googleUser?.authentication;

    // Create a new credential
    final credential = GoogleAuthProvider.credential(
      accessToken: googleAuth?.accessToken,
      idToken: googleAuth?.idToken,
    );

    // Once signed in, return the UserCredential
    return await FirebaseAuth.instance.signInWithCredential(credential);
  }

  // Sign in with email and password
  Future<UserCredential?> signInWithEmailPassword(
    String email,
    String password,
  ) async {
    try {
      return await _auth.signInWithEmailAndPassword(
        email: email,
        password: password,
      );
    } on FirebaseAuthException catch (e) {
      print('Failed with error code: ${e.code}');
      print(e.message);
      rethrow;
    }
  }

  // Register with email and password
  Future<UserCredential?> registerWithEmailPassword(
    String email,
    String password,
  ) async {
    try {
      return await _auth.createUserWithEmailAndPassword(
        email: email,
        password: password,
      );
    } on FirebaseAuthException catch (e) {
      print('Failed with error code: ${e.code}');
      print(e.message);
      rethrow;
    }
  }

  //getCurrentUserToken
  Future<String?> getCurrentUserToken() async {
    try {
      User? user = _auth.currentUser;
      if (user == null) return null;

      return await user.getIdToken();
    } on FirebaseAuthException catch (e) {
      print('Failed with error code: ${e.code}');
      print(e.message);
      rethrow;
    }
  }

  Future<void> sendEmailVerification() async {
    FirebaseAuth.instance.currentUser?.sendEmailVerification();
  }

  // Sign out
  Future<void> signOut() async {
    return await _auth.signOut();
  }

  bool isUserLoggedIn() {
    return _auth.currentUser != null;
  }

  void forceRefresh() async {
    await FirebaseAuth.instance.currentUser?.getIdToken(true);
    print(await FirebaseAuth.instance.currentUser?.getIdTokenResult(true));
  }
}
