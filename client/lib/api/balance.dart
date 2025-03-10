import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'dart:async';
import 'package:flutter/material.dart';
import './api.dart';

// Balance class remains the same
class Balance {
  final double balance;
  final DateTime updatedAt;
  final double plan;

  Balance(this.balance, this.updatedAt, this.plan);

  factory Balance.fromJson(Map<String, dynamic> json) {
    return Balance(
      _parseToDouble(json['balance']),
      DateTime.parse(json['updated_at'] as String),
      _parseToDouble(json['plan']),
    );
  }

  // Helper method to convert various numeric types to double
  static double _parseToDouble(dynamic value) {
    if (value is int) {
      return value.toDouble();
    } else if (value is double) {
      return value;
    } else if (value is String) {
      return double.tryParse(value) ?? 0.0;
    } else {
      return 0.0; // Default value if conversion fails
    }
  }
}

// Improved BalanceNotifier class
class BalanceNotifier extends AsyncNotifier<Balance?> {
  final apiService = ApiService();

  @override
  FutureOr<Balance?> build() async {
    try {
      print('BalanceNotifier: Fetching balance');
      final balance = await apiService.getBalance();
      return balance;
    } catch (e, stackTrace) {
      // Let the framework handle the error state
      throw AsyncError(e, stackTrace);
    }
  }

  // Refresh method to update the balance
  Future<void> refresh() async {
    // Use the proper state modifier method
    state = await AsyncValue.guard(() async {
      var b = await apiService.getBalance();
      print('BalanceNotifier: Fetched balance: $b');
      return b;
    });
  }

  // Getter to easily access the balance value if available
  Balance? get balance => state.valueOrNull;
}

// Provider remains the same
final balanceProvider = AsyncNotifierProvider<BalanceNotifier, Balance?>(() {
  return BalanceNotifier();
});
