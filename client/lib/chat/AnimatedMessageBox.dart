import 'package:flutter/material.dart';
import 'package:flutter_markdown/flutter_markdown.dart';
import 'package:url_launcher/url_launcher.dart';
import 'package:flutter/services.dart';
import 'package:code_highlight_view/code_highlight_view.dart';
import 'package:markdown/markdown.dart' as md;

import './message-bar.dart';
import './copy-button.dart';

// Real Visual Studio theme colors
const vsTheme = {
  'root': TextStyle(
    backgroundColor: Color(0xFF1E1E1E), // VS dark theme background
    color: Color(0xFFDCDCDC), // VS dark theme foreground
  ),
  'comment': TextStyle(color: Color(0xFF57A64A)), // Green comments
  'quote': TextStyle(color: Color(0xFF57A64A)), // Green quotes
  'variable': TextStyle(color: Color(0xFF9CDCFE)), // Light blue variables
  'keyword': TextStyle(color: Color(0xFF569CD6)), // Blue keywords
  'selector-tag': TextStyle(color: Color(0xFF569CD6)), // Blue selector tags
  'built_in': TextStyle(color: Color(0xFF4EC9B0)), // Teal built-ins
  'name': TextStyle(color: Color(0xFF569CD6)), // Blue names
  'tag': TextStyle(color: Color(0xFF569CD6)), // Blue tags
  'string': TextStyle(color: Color(0xFFD69D85)), // Light brown strings
  'title': TextStyle(color: Color(0xFFDCDCDC)), // White titles
  'section': TextStyle(color: Color(0xFFD7BA7D)), // Light brown sections
  'attribute': TextStyle(color: Color(0xFF9CDCFE)), // Light blue attributes
  'literal': TextStyle(color: Color(0xFFB5CEA8)), // Light green literals
  'template-tag': TextStyle(
    color: Color(0xFFD69D85),
  ), // Light brown template tags
  'template-variable': TextStyle(
    color: Color(0xFF9CDCFE),
  ), // Light blue template variables
  'type': TextStyle(color: Color(0xFF4EC9B0)), // Teal types
  'addition': TextStyle(color: Color(0xFF6A9955)), // Green additions
  'deletion': TextStyle(color: Color(0xFFCD3131)), // Red deletions
  'selector-attr': TextStyle(
    color: Color(0xFF9CDCFE),
  ), // Light blue selector attributes
  'selector-pseudo': TextStyle(
    color: Color(0xFFD7BA7D),
  ), // Light brown pseudo selectors
  'meta': TextStyle(color: Color(0xFF569CD6)), // Blue meta
  'doctag': TextStyle(color: Color(0xFF608B4E)), // Dark green doc tags
  'attr': TextStyle(color: Color(0xFF9CDCFE)), // Light blue attributes
  'symbol': TextStyle(color: Color(0xFFB5CEA8)), // Light green symbols
  'bullet': TextStyle(color: Color(0xFFD7BA7D)), // Light brown bullets
  'link': TextStyle(color: Color(0xFF9CDCFE)), // Light blue links
  'emphasis': TextStyle(fontStyle: FontStyle.italic), // Italic emphasis
  'strong': TextStyle(fontWeight: FontWeight.bold), // Bold strong
};

class AnimatedMessageBox extends StatefulWidget {
  final String text;
  final bool isBot;
  final bool isLoading;

  const AnimatedMessageBox({
    super.key,
    required this.text,
    required this.isBot,
    required this.isLoading,
  });

  @override
  State<AnimatedMessageBox> createState() => _AnimatedMessageBoxState();
}

class _AnimatedMessageBoxState extends State<AnimatedMessageBox>
    with SingleTickerProviderStateMixin {
  late AnimationController _animationController;

  @override
  void initState() {
    super.initState();

    _animationController = AnimationController(
      duration: const Duration(seconds: 1),
      vsync: this,
    )..repeat(reverse: true);
  }

  @override
  void dispose() {
    _animationController.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    var isDark = Theme.of(context).brightness == Brightness.dark;
    var botBG = isDark ? Color.fromRGBO(0, 0, 0, 0.35) : Colors.white;
    var botFG = isDark ? Colors.white : Colors.black;

    return MessageToolbar(
      isBot: widget.isBot,
      isHorizontal: widget.text.length > 500,
      text: widget.text,
      child: AnimatedBuilder(
        animation: _animationController,
        builder: (context, child) {
          return Stack(
            children: [
              Container(
                margin: EdgeInsets.only(
                  left: widget.isBot ? 0 : 32.0,
                  right: widget.isBot ? 32.0 : 0,
                  top: 4.0,
                  bottom: 4.0,
                ),
                padding: const EdgeInsets.all(16.0),
                decoration: BoxDecoration(
                  color:
                      widget.isBot ? botBG : Color.fromARGB(255, 204, 52, 65),
                  borderRadius: BorderRadius.circular(12.0),
                  border:
                      widget.isLoading && widget.isBot
                          ? Border.all(
                            width: 2,
                            style: BorderStyle.solid,
                            color:
                                Color.lerp(
                                  Color(0xffee4654),
                                  Color(0xfff5c256),
                                  _animationController.value,
                                )!,
                          )
                          : null,
                  boxShadow: [
                    BoxShadow(
                      color: Colors.black.withOpacity(0.1),
                      blurRadius: 4,
                      offset: const Offset(0, 2),
                    ),
                  ],
                ),
                child: SelectionArea(
                  child: MarkdownBody(
                    softLineBreak: true,
                    onTapLink: (text, url, title) {
                      if (url == null) return;

                      if (url.startsWith('http')) {
                        launchUrl(
                          Uri.parse(url ?? ''),
                          mode: LaunchMode.externalApplication,
                        );
                      } else {
                        Clipboard.setData(ClipboardData(text: url));
                      }
                    },
                    builders: {'code': CodeElementBuilder(widget.isBot)},
                    data: widget.text,
                    styleSheet: MarkdownStyleSheet(
                      p: TextStyle(color: widget.isBot ? botFG : Colors.white),
                    ),
                  ),
                ),
              ),
            ],
          );
        },
      ),
    );
  }
}

class CodeElementBuilder extends MarkdownElementBuilder {
  final bool isBot;

  CodeElementBuilder(this.isBot);

  @override
  Widget? visitElementAfter(md.Element element, TextStyle? preferredStyle) {
    if (element.tag == 'code') {
      final language = element.attributes['class']?.split('-').last ?? '';
      final code = element.textContent;
      final lines = code.split('\n');

      // Check if this is inline code (single line and no language specified or parent is not a pre tag)
      final bool isInlineCode = lines.length == 1;

      if (isInlineCode) {
        // Simple inline code styling
        return Container(
          padding: const EdgeInsets.symmetric(horizontal: 6, vertical: 2),
          decoration: BoxDecoration(
            color: isBot ? const Color(0xFFEEEEEE) : const Color(0xFF3A3A3A),
            borderRadius: BorderRadius.circular(4),
          ),
          child: Text(
            code,
            style: TextStyle(
              fontFamily: 'monospace',
              fontSize: 14,
              color: isBot ? Colors.black : Colors.white,
            ),
          ),
        );
      } else {
        // Full code block with syntax highlighting, line numbers and copy button
        return Stack(
          children: [
            Container(
              margin: const EdgeInsets.symmetric(vertical: 8),
              decoration: BoxDecoration(
                color: const Color(0xFF2D2D2D),
                borderRadius: BorderRadius.circular(4),
                border: Border.all(color: const Color(0xFF444444)),
              ),
              child: Row(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  // Line numbers column - only for multiline code
                  if (lines.length > 1)
                    Container(
                      padding: const EdgeInsets.fromLTRB(8, 8, 8, 8),
                      color: const Color(0xFF252525),
                      child: Column(
                        crossAxisAlignment: CrossAxisAlignment.end,
                        children: List.generate(
                          lines.length,
                          (index) => Text(
                            '${index + 1}',
                            style: TextStyle(
                              fontSize: 14,
                              fontFamily: 'monospace',
                              color: Colors.grey[400],
                            ),
                          ),
                        ),
                      ),
                    ),
                  // Code content
                  Expanded(
                    child: Container(
                      color: const Color(0xFF2D2D2D),
                      child: CodeHighlightView(
                        code,
                        language: language.isEmpty ? 'plaintext' : language,
                        theme: vsTheme,
                        padding: const EdgeInsets.all(8),
                        textStyle: const TextStyle(
                          fontSize: 14,
                          fontFamily: 'monospace',
                          color: Colors.white,
                        ),
                      ),
                    ),
                  ),
                ],
              ),
            ),
            // Copy button - only for code blocks
            Positioned(top: 12, right: 4, child: CopyButton(code: code)),
          ],
        );
      }
    }
    return null;
  }
}
