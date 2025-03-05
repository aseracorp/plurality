import 'package:flutter/material.dart';
import 'package:plurality/chat/copy-button.dart';

// Common button styling
final buttonStyle = IconButton.styleFrom(
  backgroundColor: Colors.white,
  foregroundColor: Colors.black87,
  elevation: 2,
  padding: const EdgeInsets.all(0),
);

class MessageToolbar extends StatelessWidget {
  final Widget child;
  final bool isHorizontal;
  final bool isBot;
  final String text;
  final bool mini;

  const MessageToolbar({
    super.key,
    required this.child,
    required this.isHorizontal,
    required this.isBot,
    required this.text,
    this.mini = false,
  });

  @override
  Widget build(BuildContext context) {
    var toolbarItems = [CopyButton(code: text)];
    // Toolbar icons
    final toolbarIcons =
        toolbarItems
            .map(
              (item) => Padding(
                padding: const EdgeInsets.symmetric(horizontal: 4.0),
                child: item,
              ),
            )
            .toList();

    if (mini) return child;

    return LayoutBuilder(
      builder: (context, constraints) {
        return Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            // Constrain child to take available width
            ConstrainedBox(
              constraints: BoxConstraints(maxWidth: constraints.maxWidth),
              child: child,
            ),
            if (isBot)
              const SizedBox(height: 4), // Spacing between content and toolbar
            if (isBot)
              Row(mainAxisSize: MainAxisSize.min, children: toolbarIcons),
          ],
        );
      },
    );
  }
}
