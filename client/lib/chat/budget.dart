import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import '../api/balance.dart';
import 'package:intl/intl.dart';
import 'package:flutter/services.dart';

// Extracted Budget Screen Widget
class BudgetScreen extends ConsumerWidget {
  const BudgetScreen({Key? key}) : super(key: key);

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final balanceState = ref.watch(balanceProvider);
    final balance = balanceState.value!;

    final percentage =
        balance.plan > 0
            ? (balance.balance / balance.plan).clamp(0.0, 1.0)
            : 0.0;

    final color =
        percentage < 0.1
            ? Colors.red
            : percentage < 0.3
            ? Colors.orange
            : Colors.lightGreen;

    return Container(
      padding: const EdgeInsets.all(20),
      width: 500,
      decoration: BoxDecoration(
        color: Colors.white,
        borderRadius: BorderRadius.circular(16),
        boxShadow: [
          BoxShadow(
            color: Colors.black.withOpacity(0.1),
            spreadRadius: 5,
            blurRadius: 15,
            offset: const Offset(0, 3),
          ),
        ],
      ),
      child: SingleChildScrollView(
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: [
            Text(
              'Budget',
              style: Theme.of(
                context,
              ).textTheme.titleLarge?.copyWith(fontWeight: FontWeight.bold),
            ),
            const SizedBox(height: 20),

            // Large Progress Indicator with percentage text
            Stack(
              alignment: Alignment.center,
              children: [
                SizedBox(
                  height: 150,
                  width: 150,
                  child: CircularProgressIndicator(
                    value: percentage,
                    strokeWidth: 12,
                    backgroundColor: Colors.grey.withOpacity(0.2),
                    valueColor: AlwaysStoppedAnimation<Color>(color),
                  ),
                ),
                Column(
                  mainAxisSize: MainAxisSize.min,
                  children: [
                    Text(
                      '${(percentage * 100).toInt()}%',
                      style: TextStyle(
                        fontSize: 32,
                        fontWeight: FontWeight.bold,
                        color: color,
                      ),
                    ),
                    Text(
                      'Left',
                      style: TextStyle(fontSize: 16, color: Colors.grey[600]),
                    ),
                  ],
                ),
              ],
            ),

            const SizedBox(height: 24),

            // Balance and Plan Details
            Container(
              padding: const EdgeInsets.all(16),
              decoration: BoxDecoration(
                color: Colors.grey[100],
                borderRadius: BorderRadius.circular(12),
              ),
              child: Column(
                children: [
                  // Current Usage
                  Row(
                    mainAxisAlignment: MainAxisAlignment.spaceBetween,
                    children: [
                      Text(
                        'Credits Left',
                        style: TextStyle(fontSize: 16, color: Colors.grey[800]),
                      ),
                      Text(
                        NumberFormat.decimalPattern().format(balance.balance),
                        style: TextStyle(
                          fontSize: 16,
                          fontWeight: FontWeight.bold,
                          color: color,
                        ),
                      ),
                    ],
                  ),
                  const SizedBox(height: 12),

                  // Total Budget
                  Row(
                    mainAxisAlignment: MainAxisAlignment.spaceBetween,
                    children: [
                      Text(
                        'Credits Plan',
                        style: TextStyle(fontSize: 16, color: Colors.grey[800]),
                      ),
                      Text(
                        NumberFormat.decimalPattern().format(balance.plan),
                        style: const TextStyle(
                          fontSize: 16,
                          fontWeight: FontWeight.bold,
                        ),
                      ),
                    ],
                  ),
                ],
              ),
            ),

            const SizedBox(height: 16),

            // Balance and Plan Details
            Container(
              padding: const EdgeInsets.all(16),
              decoration: BoxDecoration(
                color: Colors.grey[100],
                borderRadius: BorderRadius.circular(12),
              ),
              child: Column(
                children: [
                  Text(
                    'Estimation of your remaining budget based on the default AI present',
                    style: TextStyle(fontSize: 16, color: Colors.grey[800]),
                  ),

                  const SizedBox(height: 16),

                  // estimated usage in term of number of message sent
                  Row(
                    mainAxisAlignment: MainAxisAlignment.spaceBetween,
                    children: [
                      Text(
                        'Messages',
                        style: TextStyle(fontSize: 14, color: Colors.grey[800]),
                      ),
                      Text(
                        NumberFormat.decimalPattern().format(
                              (balance.balance / 1250).toInt(),
                            ) +
                            ' - ' +
                            NumberFormat.decimalPattern().format(
                              (balance.balance / 750).toInt(),
                            ),
                        style: TextStyle(
                          fontSize: 16,
                          fontWeight: FontWeight.bold,
                          color: color,
                        ),
                      ),
                    ],
                  ),
                  // estimated usage in term of number of message sent
                  Row(
                    mainAxisAlignment: MainAxisAlignment.spaceBetween,
                    children: [
                      Text(
                        'Images Generation',
                        style: TextStyle(fontSize: 14, color: Colors.grey[800]),
                      ),
                      Text(
                        NumberFormat.decimalPattern().format(
                              (balance.balance / 4000).toInt(),
                            ) +
                            ' - ' +
                            NumberFormat.decimalPattern().format(
                              (balance.balance / 3500).toInt(),
                            ),
                        style: TextStyle(
                          fontSize: 16,
                          fontWeight: FontWeight.bold,
                          color: color,
                        ),
                      ),
                    ],
                  ),
                ],
              ),
            ),

            const SizedBox(height: 16),

            // Last Updated
            Text(
              'Last updated: ${DateFormat('MMM d, yyyy - HH:mm').format(balance.updatedAt)}',
              style: TextStyle(
                fontSize: 12,
                color: Colors.grey[600],
                fontStyle: FontStyle.italic,
              ),
            ),

            const SizedBox(height: 20),

            // Close button for dialog mode
            if (Navigator.of(context).canPop())
              TextButton(
                onPressed: () => Navigator.of(context).pop(),
                style: TextButton.styleFrom(
                  foregroundColor: Colors.white,
                  backgroundColor: Theme.of(context).primaryColor,
                  padding: const EdgeInsets.symmetric(
                    horizontal: 30,
                    vertical: 10,
                  ),
                  shape: RoundedRectangleBorder(
                    borderRadius: BorderRadius.circular(20),
                  ),
                ),
                child: const Text('Close'),
              ),
          ],
        ),
      ),
    );
  }
}

// Main Balance Progress Circle widget with clickable prop
class BalanceProgressCircle extends ConsumerWidget {
  final double size;
  final Color progressColor;
  final Color backgroundColor;
  final double strokeWidth;
  final bool isClickable;

  const BalanceProgressCircle({
    Key? key,
    this.size = 24.0,
    this.progressColor = Colors.blue,
    this.backgroundColor = Colors.grey,
    this.strokeWidth = 4.0,
    this.isClickable = true,
  }) : super(key: key);

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    // Watch the balance provider
    final balanceState = ref.watch(balanceProvider);

    var mainContent = Padding(
      padding: const EdgeInsets.all(4.0), // Add padding for larger touch target
      child: SizedBox(
        width: size,
        height: size,
        child: balanceState.when(
          data: (balance) {
            if (balance == null) {
              return _buildEmptyCircle();
            }
            // Calculate percentage (balance / plan)
            // Ensure we don't exceed 100% for display purposes
            final percentage =
                balance.plan > 0
                    ? (balance.balance / balance.plan).clamp(0.0, 1.0)
                    : 0.0;
            final color =
                percentage < 0.1
                    ? Colors.red
                    : percentage < 0.3
                    ? Colors.orange
                    : Colors.lightGreen;
            return _buildProgressCircle(percentage, color);
          },
          loading: () => const CircularProgressIndicator(strokeWidth: 2.0),
          error: (error, stackTrace) {
            print('Error: $error');
            return _buildErrorCircle();
          },
        ),
      ),
    );

    return Material(
      color: Colors.transparent,
      child:
          isClickable
              ? InkWell(
                borderRadius: BorderRadius.circular(size / 2),
                splashColor: Colors.grey.withOpacity(0.3),
                highlightColor: Colors.grey.withOpacity(0.1),
                onTap: () {
                  if (balanceState is AsyncData && balanceState.value != null) {
                    _showBudgetBreakdownModal(context, balanceState.value!);
                    // Add haptic feedback for better interaction
                    HapticFeedback.lightImpact();
                  }
                },
                child: mainContent,
              )
              : mainContent,
    );
  }

  // Show the budget breakdown modal
  void _showBudgetBreakdownModal(BuildContext context, Balance balance) {
    showDialog(
      context: context,
      builder: (BuildContext context) {
        return Dialog(
          shape: RoundedRectangleBorder(
            borderRadius: BorderRadius.circular(16),
          ),
          elevation: 0,
          backgroundColor: Colors.transparent,
          child: BudgetScreen(),
        );
      },
    );
  }

  Widget _buildProgressCircle(double percentage, Color progressColor) {
    return Stack(
      children: [
        Positioned(
          top: size * 0.5 - 7,
          left: 5,
          child: Icon(
            Icons.account_balance_wallet_outlined,
            size: 13,
            color: Colors.grey,
          ),
        ),
        Center(
          child: CircularProgressIndicator(
            value: 1.0,
            strokeWidth: strokeWidth,
            backgroundColor: backgroundColor.withOpacity(0.3),
            valueColor: AlwaysStoppedAnimation<Color>(backgroundColor),
          ),
        ),
        // Progress indicator
        Center(
          child: CircularProgressIndicator(
            value: percentage,
            strokeWidth: strokeWidth,
            color: progressColor,
            backgroundColor: Colors.transparent,
            valueColor: AlwaysStoppedAnimation<Color>(progressColor),
          ),
        ),
      ],
    );
  }

  Widget _buildEmptyCircle() {
    return CircleAvatar(
      radius: size / 2,
      backgroundColor: backgroundColor.withOpacity(0.2),
      child: Icon(
        Icons.account_balance_wallet_outlined,
        size: size * 0.6,
        color: backgroundColor,
      ),
    );
  }

  Widget _buildErrorCircle() {
    return CircleAvatar(
      radius: size / 2,
      backgroundColor: Colors.red.withOpacity(0.2),
      child: Icon(Icons.error_outline, size: size * 0.6, color: Colors.red),
    );
  }
}
