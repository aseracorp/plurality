import 'dart:convert';
import 'package:flutter/material.dart';
import 'package:file_saver/file_saver.dart';
import 'package:share_plus/share_plus.dart';

class ImagePreviewComponent extends StatelessWidget {
  final String imageUrl;
  final bool mini;

  const ImagePreviewComponent({
    Key? key,
    required this.imageUrl,
    this.mini = false,
  }) : super(key: key);

  @override
  Widget build(BuildContext context) {
    // Extract the base64 image data
    final imageData = imageUrl.split(",").last;

    return Padding(
      key: ValueKey('image_${imageUrl.hashCode}'),
      padding: const EdgeInsets.only(bottom: 8.0),
      child: GestureDetector(
        onTap: () => {if (!mini) _showImagePreviewModal(context, imageData)},
        child: ClipRRect(
          borderRadius:
              mini ? BorderRadius.circular(2.0) : BorderRadius.circular(8.0),
          child: SizedBox(
            height: mini ? 20 : 200,
            child: Image.memory(
              base64Decode(imageData),
              height: mini ? 20 : 200,
              fit: BoxFit.cover,
              cacheWidth: 200,
              gaplessPlayback: true,
            ),
          ),
        ),
      ),
    );
  }

  void _showImagePreviewModal(BuildContext context, String imageData) {
    // First, decode the image to get its dimensions
    final decodedImage = base64Decode(imageData);

    showDialog(
      context: context,
      builder: (BuildContext context) {
        return Dialog(
          // Remove default inset padding to allow custom sizing
          insetPadding: EdgeInsets.zero,
          // Make the dialog background transparent
          backgroundColor: Colors.transparent,
          // Use IntrinsicWidth to make the dialog only as wide as its content
          child: IntrinsicWidth(
            child: Column(
              mainAxisSize: MainAxisSize.min,
              crossAxisAlignment: CrossAxisAlignment.stretch,
              children: [
                // Custom app bar with same width as image
                Container(
                  color: Colors.black.withOpacity(0.7),
                  padding: const EdgeInsets.symmetric(
                    vertical: 8,
                    horizontal: 8,
                  ),
                  child: Row(
                    mainAxisAlignment: MainAxisAlignment.spaceBetween,
                    children: [
                      IconButton(
                        icon: const Icon(Icons.close, color: Colors.white),
                        onPressed: () => Navigator.of(context).pop(),
                        padding: EdgeInsets.zero,
                        constraints: const BoxConstraints(),
                        iconSize: 20,
                      ),
                      const Text(
                        'Image Preview',
                        style: TextStyle(color: Colors.white),
                      ),
                      Row(
                        children: [
                          IconButton(
                            icon: const Icon(
                              Icons.download,
                              color: Colors.white,
                            ),
                            onPressed: () async {
                              await FileSaver.instance.saveFile(
                                name:
                                    'image_${DateTime.now().millisecondsSinceEpoch}.jpg',
                                bytes: decodedImage,
                              );

                              ScaffoldMessenger.of(context).showSnackBar(
                                const SnackBar(
                                  content: Text(
                                    'Image saved to download folder',
                                  ),
                                ),
                              );
                            },
                            padding: EdgeInsets.zero,
                            constraints: const BoxConstraints(),
                            iconSize: 20,
                          ),
                          VerticalDivider(width: 10),
                          IconButton(
                            icon: const Icon(Icons.share, color: Colors.white),
                            onPressed: () async {
                              await Share.shareXFiles(
                                [XFile.fromData(decodedImage)],
                                fileNameOverrides: ['image.jpg'],
                              );
                            },
                            padding: EdgeInsets.zero,
                            constraints: const BoxConstraints(),
                            iconSize: 20,
                          ),
                        ],
                      ),
                    ],
                  ),
                ),
                // Wrap the InteractiveViewer in a container with constraints to handle large images
                Container(
                  constraints: BoxConstraints(
                    maxWidth: MediaQuery.of(context).size.width * 0.9,
                    maxHeight: MediaQuery.of(context).size.height * 0.8,
                  ),
                  decoration: BoxDecoration(
                    color: Colors.black.withOpacity(0.5),
                  ),
                  child: InteractiveViewer(
                    minScale: 0.5,
                    maxScale: 4.0,
                    child: Image.memory(
                      decodedImage,
                      fit: BoxFit.contain,
                      gaplessPlayback: true,
                    ),
                  ),
                ),
              ],
            ),
          ),
        );
      },
    );
  }
}
