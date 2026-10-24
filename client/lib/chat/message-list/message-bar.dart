import 'package:flutter/material.dart';
import 'package:plurality/chat/message-list/copy-button.dart';
import '../../utils/types.dart';
import 'dart:convert';
import 'dart:typed_data';

// Common button styling
final buttonStyle = IconButton.styleFrom(
  backgroundColor: Colors.white,
  foregroundColor: Colors.black87,
  elevation: 2,
  padding: const EdgeInsets.all(0),
);

// Decode-once cache for bot avatar icons. Image.memory keys its image cache
// on byte identity, so reusing the same Uint8List instance across rebuilds
// avoids re-decoding and re-uploading the texture to the GPU on every build
// (and per streaming chunk, where the toolbar ValueKey changes with the text).
final Map<String, Uint8List> _avatarCache = {};
Uint8List _avatarBytes(String url) =>
    _avatarCache.putIfAbsent(url, () => base64Decode(url));

class MessageToolbar extends StatelessWidget {
  final Widget child;
  final bool isHorizontal;
  final bool isBot;
  final String text;
  final bool mini;
  final Function()? ttsCallback;
  final bool isSpeaking;
  final Message message;
  final String? iconURL;
  final bool isLoading;

  const MessageToolbar({
    super.key,
    required this.child,
    this.iconURL,
    required this.isHorizontal,
    required this.isLoading,
    required this.isBot,
    required this.text,
    this.mini = false,
    this.ttsCallback,
    required this.message,
    this.isSpeaking = false,
  });

  @override
  Widget build(BuildContext context) {
    var toolbarItems = [
      CopyButton(code: text),
      if (ttsCallback != null) // Only show TTS button if callback is provided
        TTSButton(onPressed: ttsCallback!, isSpeaking: isSpeaking),
      // Add the info button
      if (isBot) InfoButton(message: message),
    ];

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
      key:
          isLoading
              ? ValueKey('message_toolbar_loading')
              : ValueKey('message_toolbar_${text.hashCode}'),
      builder: (context, constraints) {
        return Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment:
              isBot ? CrossAxisAlignment.start : CrossAxisAlignment.end,
          children: [
            if (iconURL != null && isBot)
              // RepaintBoundary isolates the avatar from the animated border
              // and list scrolling; the cached bytes keep MemoryImage stable
              // so it is decoded/uploaded to the GPU exactly once.
              RepaintBoundary(
                child: Container(
                  key:
                      isLoading
                          ? ValueKey(
                            'image_message_toolbar_loading_${iconURL.hashCode}',
                          )
                          : ValueKey(
                            'image_${iconURL.hashCode}_${text.hashCode}',
                          ),
                  margin: const EdgeInsets.only(bottom: 8),
                  width: 48,
                  decoration: BoxDecoration(
                    borderRadius: BorderRadius.circular(1000.0),
                    boxShadow: [
                      BoxShadow(
                        color: Colors.black.withOpacity(0.1),
                        blurRadius: 23,
                        offset: const Offset(0, 2),
                      ),
                    ],
                  ),
                  clipBehavior: Clip.antiAlias,
                  child: Image.memory(
                    _avatarBytes(iconURL!),
                    width: 48,
                    height: 48,
                    fit: BoxFit.cover,
                    gaplessPlayback: true,
                  ),
                ),
              ),

            // Constrain child to take available width
            ConstrainedBox(
              constraints: BoxConstraints(maxWidth: constraints.maxWidth),
              child: child,
            ),
            const SizedBox(height: 4), // Spacing between content and toolbar
            Row(mainAxisSize: MainAxisSize.min, children: toolbarIcons),
          ],
        );
      },
    );
  }
}

// New TTS Button widget
class TTSButton extends StatelessWidget {
  final VoidCallback onPressed;
  final bool isSpeaking;

  const TTSButton({Key? key, required this.onPressed, required this.isSpeaking})
    : super(key: key);

  @override
  Widget build(BuildContext context) {
    return IconButton(
      iconSize: 16,
      padding: EdgeInsets.zero,
      onPressed: onPressed,
      style: IconButton.styleFrom(
        backgroundColor: isSpeaking ? Theme.of(context).primaryColor : null,
        shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(36)),
        padding: const EdgeInsets.all(12),
      ),
      icon: Icon(
        isSpeaking ? Icons.volume_off : Icons.volume_up,
        color: isSpeaking ? Theme.of(context).colorScheme.onPrimary : null,
      ),
      tooltip: isSpeaking ? 'Stop speaking' : 'Read aloud',
    );
  }
}

// New Info Button widget
class InfoButton extends StatelessWidget {
  final Message message;

  const InfoButton({Key? key, required this.message}) : super(key: key);

  @override
  Widget build(BuildContext context) {
    return IconButton(
      iconSize: 16,
      padding: EdgeInsets.zero,
      onPressed: () {
        _showTokenInfoModal(context);
      },
      style: IconButton.styleFrom(
        shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(36)),
        padding: const EdgeInsets.all(12),
      ),
      icon: const Icon(Icons.info_outline),
      tooltip: 'message info',
    );
  }

  void _showTokenInfoModal(BuildContext context) {
    final hasCost =
        message.responseCost != null && message.responseCost! > 0;
    showDialog(
      context: context,
      builder: (BuildContext context) {
        return AlertDialog(
          title: const Text('Message Information'),
          content: Column(
            mainAxisSize: MainAxisSize.min,
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Text(
                'Tokens:',
                style: const TextStyle(fontWeight: FontWeight.bold),
              ),
              Text('Input: ${message.promptTokens ?? "?"}'),
              Text('Output: ${message.completionTokens ?? "?"}'),
              Text('Total: ${message.totalTokens ?? "?"}'),
              if (hasCost) ...[
                const SizedBox(height: 8),
                Text(
                  'Cost:',
                  style: const TextStyle(fontWeight: FontWeight.bold),
                ),
                Text('\$${message.responseCost!.toStringAsFixed(4)}'),
              ],
              const SizedBox(height: 8),
              Text(
                'Model used:',
                style: const TextStyle(fontWeight: FontWeight.bold),
              ),
              Text('${message.model?.name ?? "?"}'),
              if (message.model?.params != null)
                Text(
                  'Parameters:',
                  style: const TextStyle(fontWeight: FontWeight.bold),
                ),
              // for each message.model.param key value
              if (message.model?.params != null)
                for (var key in message.model!.params?.keys ?? [])
                  Text('$key: ${message.model!.params![key]}'),
            ],
          ),
          actions: [
            TextButton(
              onPressed: () {
                Navigator.of(context).pop();
              },
              child: const Text('Close'),
            ),
          ],
        );
      },
    );
  }
}
