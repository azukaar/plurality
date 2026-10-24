import 'package:flutter/material.dart';
import 'package:flutter_markdown/flutter_markdown.dart';
import 'package:url_launcher/url_launcher.dart';
import 'package:flutter/services.dart';
import 'package:code_highlight_view/code_highlight_view.dart';
import 'package:markdown/markdown.dart' as md;
import 'dart:async';

import 'message-bar.dart';
import 'copy-button.dart';
import '../../api/tts.dart';
import '../../utils/types.dart';

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
  ), // Light brown selector pseudo
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
  final bool mini;
  final Message message;
  final String? iconURL;
  final Function(String)? onConversationTap;

  const AnimatedMessageBox({
    super.key,
    this.iconURL,
    required this.text,
    required this.isBot,
    required this.isLoading,
    this.mini = false,
    required this.message,
    this.onConversationTap,
  });

  @override
  State<AnimatedMessageBox> createState() => _AnimatedMessageBoxState();
}

class _AnimatedMessageBoxState extends State<AnimatedMessageBox>
    with SingleTickerProviderStateMixin {
  late AnimationController _animationController;
  final TTSService _ttsService = TTSService();
  late StreamSubscription<bool> _speakingSubscription;
  bool _isSpeaking = false;

  // Only the currently-streaming bot message should pulse its border. Starting
  // the repeat unconditionally kept every finished message animating forever
  // at 60fps, re-laying-out the whole markdown subtree each frame (CanvasKit =
  // WebGL GPU work, ReadPixels stalls, OOM in Firefox).
  bool get _shouldAnimate => widget.isLoading && widget.isBot;

  @override
  void initState() {
    super.initState();

    _animationController = AnimationController(
      duration: const Duration(seconds: 1),
      vsync: this,
    );
    if (_shouldAnimate) {
      _animationController.repeat(reverse: true);
    }

    _ttsService.initialize();

    // Listen to speaking state changes
    _speakingSubscription = _ttsService.speakingState.listen((speaking) {
      // Only update if this message is the one being spoken
      if (_ttsService.currentText == widget.text || !speaking) {
        setState(() {
          _isSpeaking = speaking;
        });
      }
    });

    // Check if this message is currently being spoken
    _isSpeaking =
        _ttsService.isSpeaking && _ttsService.currentText == widget.text;
  }

  // on prop change
  @override
  void didUpdateWidget(AnimatedMessageBox oldWidget) {
    super.didUpdateWidget(oldWidget);

    // Start the pulse when a message starts streaming, stop it when it
    // finishes (previous code only stopped, leaving finished bubbles
    // animating forever).
    if (_shouldAnimate) {
      if (!_animationController.isAnimating) {
        _animationController.repeat(reverse: true);
      }
    } else {
      _animationController.stop();
    }
  }

  Future<void> speak() async {
    if (widget.text.isNotEmpty) {
      _ttsService.speak(widget.text, null);
    }
  }

  Future<void> stop() async {
    await _ttsService.stop();
  }

  @override
  void dispose() {
    _animationController.dispose();
    _speakingSubscription.cancel();
    super.dispose();
  }

  /// The heavy markdown subtree. Built once per widget build and handed to
  /// AnimatedBuilder as its `child`, so per-frame ticks never re-layout it.
  Widget _buildMessageContent({
    required bool isDark,
    required Color botBG,
    required Color botFG,
    required Color userBG,
  }) {
    return SelectionArea(
      child: MarkdownBody(
        softLineBreak: true,
        onTapLink: (text, url, title) {
          if (url == null) return;

          if (url.startsWith('plurality://conversation/')) {
            final convId = url.replaceFirst('plurality://conversation/', '');
            if (widget.onConversationTap != null) {
              widget.onConversationTap!(convId);
            }
          } else if (url.startsWith('http')) {
            launchUrl(
              Uri.parse(url ?? ''),
              mode: LaunchMode.externalApplication,
            );
          } else {
            Clipboard.setData(ClipboardData(text: url));
          }
        },
        builders: {
          'code': CodeElementBuilder(
            widget.isBot,
            isDark,
            mini: widget.mini,
          ),
        },
        data: widget.text,
        styleSheet: MarkdownStyleSheet(
          p: TextStyle(
            color: widget.isBot ? botFG : Colors.white,
            fontSize: widget.mini ? 3 : 16,
            height: widget.mini ? 1.0 : 1.5,
          ),
          pPadding: EdgeInsets.only(bottom: widget.mini ? 0 : 6),
          h1: TextStyle(fontSize: widget.mini ? 6 : 24),
          h1Padding: EdgeInsets.only(top: widget.mini ? 0 : 18, bottom: widget.mini ? 0 : 8),
          h2: TextStyle(fontSize: widget.mini ? 5 : 20),
          h2Padding: EdgeInsets.only(top: widget.mini ? 0 : 16, bottom: widget.mini ? 0 : 6),
          h3: TextStyle(fontSize: widget.mini ? 4 : 18),
          h3Padding: EdgeInsets.only(top: widget.mini ? 0 : 14, bottom: widget.mini ? 0 : 6),
          h4: TextStyle(fontSize: widget.mini ? 4 : 16),
          h5: TextStyle(fontSize: widget.mini ? 4 : 14),
          h6: TextStyle(fontSize: widget.mini ? 4 : 12),
          blockquote: TextStyle(
            color: (widget.isBot ? botFG : Colors.white).withOpacity(0.75),
            fontSize: widget.mini ? 3 : 16,
            height: widget.mini ? 1.0 : 1.5,
            fontStyle: FontStyle.italic,
          ),
          listBullet: TextStyle(fontSize: widget.mini ? 3 : 16),
          blockquotePadding: EdgeInsets.only(
            left: widget.mini ? 4 : 12,
            right: widget.mini ? 2 : 8,
            top: widget.mini ? 1 : 4,
            bottom: widget.mini ? 1 : 4,
          ),
          blockquoteDecoration: BoxDecoration(
            color: (widget.isBot ? botFG : Colors.white).withOpacity(0.06),
            border: Border(
              left: BorderSide(
                color: (widget.isBot ? botFG : Colors.white).withOpacity(0.35),
                width: widget.mini ? 1 : 3,
              ),
            ),
            borderRadius: BorderRadius.circular(widget.mini ? 1 : 4),
          ),
          listBulletPadding: EdgeInsets.only(
            left: widget.mini ? 0 : (MediaQuery.of(context).size.width >= 600 ? 8 : 2),
            right: widget.mini ? 0 : (MediaQuery.of(context).size.width >= 600 ? 8 : 4),
            top: 0,
            bottom: 0,
          ),
          listIndent: widget.mini ? 1 : (MediaQuery.of(context).size.width >= 600 ? 24 : 12),
          codeblockPadding: EdgeInsets.symmetric(vertical: widget.mini ? 0 : 8),
          tableHead: TextStyle(fontSize: widget.mini ? 3 : 16),
          tableBody: TextStyle(fontSize: widget.mini ? 3 : 16),
          tablePadding: EdgeInsets.symmetric(
            horizontal: widget.mini ? 0 : 8,
            vertical: widget.mini ? 0 : 16,
          ),
          tableCellsPadding: EdgeInsets.symmetric(
            horizontal: widget.mini ? 2 : 8,
            vertical: widget.mini ? 1 : 8,
          ),
          horizontalRuleDecoration: BoxDecoration(
            border: Border(
              top: BorderSide(color: Colors.transparent, width: widget.mini ? 0 : 12),
              bottom: BorderSide(
                color: (widget.isBot ? botFG : Colors.white).withOpacity(0.2),
                width: 1,
              ),
            ),
          ),
          blockSpacing: widget.mini ? 0 : 16,
          codeblockDecoration: BoxDecoration(
            color: Colors.transparent,
            borderRadius: BorderRadius.circular(0),
          ),
        ),
      ),
    );
  }

  @override
  Widget build(BuildContext context) {
    var isDark = Theme.of(context).brightness == Brightness.dark;
    var botBG = isDark ? Color.fromARGB(15, 255, 255, 255) : Colors.white;
    var botFG = isDark ? Colors.white : Colors.black;
    var userBG =
        isDark
            ? Theme.of(context).colorScheme.surfaceBright
            : Theme.of(context).primaryColor;

    return MessageToolbar(
      key: ValueKey('message_xtoolbar_${widget.text.hashCode}'),
      iconURL: widget.iconURL,
      message: widget.message,
      isBot: widget.isBot,
      isHorizontal: widget.text.length > 500,
      text: widget.text,
      isLoading: widget.isLoading,
      mini: widget.mini,
      // Add the TTS functionality to the MessageToolbar
      ttsCallback: _isSpeaking ? stop : speak,
      isSpeaking: _isSpeaking,
      // The markdown body is built once per widget build and reused as the
      // AnimatedBuilder child, so animation ticks only rebuild the lightweight
      // border Container. RepaintBoundary keeps the 60fps border pulse from
      // re-rasterizing the markdown layer on CanvasKit/WebGL.
      child: AnimatedBuilder(
        animation: _animationController,
        child: _buildMessageContent(
          isDark: isDark,
          botBG: botBG,
          botFG: botFG,
          userBG: userBG,
        ),
        builder: (context, child) {
          return RepaintBoundary(
            child: Stack(
              children: [
                Container(
                  margin: EdgeInsets.only(
                    left: widget.isBot ? 0 : 32.0,
                    right: widget.isBot ? 32.0 : 8.0,
                    top: 4.0,
                    bottom: 4.0,
                  ),
                  padding: EdgeInsets.symmetric(
                    vertical: widget.mini ? 1.0 : (MediaQuery.of(context).size.width >= 600 ? 16.0 : 16.0),
                    horizontal: widget.mini ? 1.0 : (MediaQuery.of(context).size.width >= 600 ? 24.0 : 16.0),
                  ),
                  decoration: BoxDecoration(
                    color: widget.isBot ? botBG : userBG,
                    borderRadius:
                        widget.mini
                            ? BorderRadius.circular(2.0)
                            : BorderRadius.circular(12.0),
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
                    boxShadow:
                        widget.mini
                            ? [
                              BoxShadow(
                                color: Colors.black.withOpacity(0.1),
                                blurRadius: 4,
                                offset: const Offset(0, 2),
                              ),
                            ]
                            : null,
                  ),
                  child: RepaintBoundary(child: child!),
                ),
              ],
            ),
          );
        },
      ),
    );
  }
}

class CodeElementBuilder extends MarkdownElementBuilder {
  final bool isBot;
  final bool mini;
  final bool isDark;

  CodeElementBuilder(this.isBot, this.isDark, {this.mini = false});

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
          padding:
              mini
                  ? const EdgeInsets.symmetric(horizontal: 2, vertical: 1)
                  : const EdgeInsets.symmetric(horizontal: 6, vertical: 2),
          decoration: BoxDecoration(
            color: Color.fromARGB(255, 212, 212, 212),
            borderRadius: BorderRadius.circular(4),
          ),
          child: Text(
            code,
            style: TextStyle(
              fontFamily: 'monospace',
              fontSize: mini ? 3 : 14,
              color: const Color.fromARGB(255, 0, 0, 0),
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
                borderRadius: BorderRadius.circular(mini ? 2 : 4),
                border: Border.all(color: const Color(0xFF444444)),
              ),
              child: Row(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  // Line numbers column - only for multiline code
                  if (lines.length > 1 && !mini)
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
                              fontSize: mini ? 3 : 14,
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
                        textStyle: TextStyle(
                          fontSize: mini ? 3 : 14,
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
            if (!mini)
              Positioned(
                top: 12,
                right: 4,
                child: CopyButton(code: code, isLight: true),
              ),
          ],
        );
      }
    }
    return null;
  }
}
