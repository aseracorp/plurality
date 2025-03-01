import 'package:flutter/material.dart';

enum SnackbarType { success, error, info, warning }

class PrettySnackbar extends StatelessWidget {
  final String message;
  final SnackbarType type;
  final VoidCallback? onClose;

  const PrettySnackbar({
    super.key,
    required this.message,
    this.type = SnackbarType.info,
    this.onClose,
  });

  Color get backgroundColor {
    switch (type) {
      case SnackbarType.success:
        return Colors.green.shade50;
      case SnackbarType.error:
        return Colors.red.shade50;
      case SnackbarType.warning:
        return Colors.orange.shade50;
      case SnackbarType.info:
        return Colors.blue.shade50;
    }
  }

  Color get iconColor {
    switch (type) {
      case SnackbarType.success:
        return Colors.green;
      case SnackbarType.error:
        return Colors.red;
      case SnackbarType.warning:
        return Colors.orange;
      case SnackbarType.info:
        return Colors.blue;
    }
  }

  IconData get icon {
    switch (type) {
      case SnackbarType.success:
        return Icons.check_circle;
      case SnackbarType.error:
        return Icons.error;
      case SnackbarType.warning:
        return Icons.warning;
      case SnackbarType.info:
        return Icons.info;
    }
  }

  @override
  Widget build(BuildContext context) {
    return Container(
      margin: const EdgeInsets.symmetric(horizontal: 16, vertical: 8),
      decoration: BoxDecoration(
        color: backgroundColor,
        borderRadius: BorderRadius.circular(12),
        boxShadow: [
          BoxShadow(
            color: Colors.black.withOpacity(0.1),
            blurRadius: 8,
            offset: const Offset(0, 2),
          ),
        ],
        border: Border.all(
          color: iconColor.withOpacity(0.2),
        ),
      ),
      child: Material(
        color: Colors.transparent,
        child: Padding(
          padding: const EdgeInsets.all(12),
          child: Row(
            crossAxisAlignment: CrossAxisAlignment.center,
            children: [
              Icon(
                icon,
                color: iconColor,
                size: 24,
              ),
              const SizedBox(width: 12),
              Expanded(
                child: Center(
                  // Added Center widget here
                  child: Text(
                    message,
                    style: TextStyle(
                      color: iconColor.withOpacity(0.8),
                      fontSize: 14,
                      fontWeight: FontWeight.w500,
                    ),
                  ),
                ),
              ),
              const SizedBox(width: 12),
              GestureDetector(
                onTap: onClose,
                behavior: HitTestBehavior.opaque,
                child: Padding(
                  padding: const EdgeInsets.all(4),
                  child: Icon(
                    Icons.close,
                    color: iconColor.withOpacity(0.5),
                    size: 20,
                  ),
                ),
              ),
            ],
          ),
        ),
      ),
    );
  }
}
