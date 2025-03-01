import 'package:flutter/material.dart';
import 'package:flutter/services.dart';

class CopyButton extends StatefulWidget {
  final String code;
  const CopyButton({Key? key, required this.code}) : super(key: key);

  @override
  State<CopyButton> createState() => _CopyButtonState();
}

class _CopyButtonState extends State<CopyButton>
    with SingleTickerProviderStateMixin {
  late AnimationController _controller;
  late Animation<Color?> _colorAnimation;
  late Animation<Color?> _iconColorAnimation;

  @override
  void initState() {
    super.initState();
    _controller = AnimationController(
      duration: const Duration(milliseconds: 800),
      vsync: this,
    );

    _colorAnimation = ColorTween(
      begin: Colors.white,
      end: const Color.fromARGB(255, 70, 238, 112),
    ).animate(
      CurvedAnimation(
        parent: _controller,
        curve: const Interval(0.0, 0.1, curve: Curves.easeOutQuint),
        reverseCurve: const Interval(0.3, 1.0, curve: Curves.easeInOutCubic),
      ),
    );

    _iconColorAnimation = ColorTween(
      begin: Colors.black87,
      end: Colors.white,
    ).animate(
      CurvedAnimation(
        parent: _controller,
        curve: const Interval(0.0, 0.1, curve: Curves.easeOutQuint),
        reverseCurve: const Interval(0.3, 1.0, curve: Curves.easeInOutCubic),
      ),
    );

    _controller.addStatusListener((status) {
      if (status == AnimationStatus.completed) {
        _controller.reverse();
      }
    });
  }

  @override
  void dispose() {
    _controller.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return AnimatedBuilder(
      animation: _controller,
      builder: (context, child) {
        return IconButton(
          icon: Icon(Icons.copy, size: 16, color: _iconColorAnimation.value),
          style: IconButton.styleFrom(
            backgroundColor: _colorAnimation.value,
            side: const BorderSide(color: Colors.white),
            shape: RoundedRectangleBorder(
              borderRadius: BorderRadius.circular(36),
            ),
            padding: const EdgeInsets.all(12),
          ),
          onPressed: () {
            Clipboard.setData(ClipboardData(text: widget.code));
            _controller.forward();
          },
        );
      },
    );
  }
}
