import 'package:flutter/material.dart';
import 'package:flutter/services.dart';

class CopyButton extends StatefulWidget {
  final String code;
  final bool isLight;
  const CopyButton({Key? key, required this.code, this.isLight = false})
    : super(key: key);

  @override
  State<CopyButton> createState() => _CopyButtonState();
}

class _CopyButtonState extends State<CopyButton> {
  @override
  Widget build(BuildContext context) {
    return IconButton(
      icon: Icon(Icons.copy, size: 16),
      style: IconButton.styleFrom(
        backgroundColor: widget.isLight ? Colors.white : null,
        shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(36)),
        padding: const EdgeInsets.all(12),
      ),
      onPressed: () {
        Clipboard.setData(ClipboardData(text: widget.code));
        ScaffoldMessenger.of(context).showSnackBar(
          SnackBar(
            content: Text('Copied to clipboard'),
            duration: const Duration(seconds: 1),
          ),
        );
      },
    );
  }
}
