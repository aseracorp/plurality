import 'dart:typed_data';
import 'package:flutter/material.dart';
import 'package:file_saver/file_saver.dart';
import 'package:share_plus/share_plus.dart';
import '../../utils/image_loader.dart';

class ImagePreviewComponent extends StatefulWidget {
  final String imageUrl;
  final bool mini;

  const ImagePreviewComponent({
    Key? key,
    required this.imageUrl,
    this.mini = false,
  }) : super(key: key);

  @override
  State<ImagePreviewComponent> createState() => _ImagePreviewComponentState();
}

class _ImagePreviewComponentState extends State<ImagePreviewComponent> {
  Future<Uint8List>? _imageFuture;

  @override
  void initState() {
    super.initState();
    _imageFuture = loadImageBytes(widget.imageUrl);
  }

  @override
  void didUpdateWidget(ImagePreviewComponent oldWidget) {
    super.didUpdateWidget(oldWidget);
    if (oldWidget.imageUrl != widget.imageUrl) {
      _imageFuture = loadImageBytes(widget.imageUrl);
    }
  }

  @override
  Widget build(BuildContext context) {
    return Padding(
      key: ValueKey('image_${widget.imageUrl.hashCode}'),
      padding: const EdgeInsets.only(bottom: 8.0),
      child: FutureBuilder<Uint8List>(
        future: _imageFuture,
        builder: (context, snapshot) {
          if (snapshot.connectionState == ConnectionState.waiting) {
            return SizedBox(
              height: widget.mini ? 20 : 200,
              width: widget.mini ? 20 : 200,
              child: const Center(child: CircularProgressIndicator(strokeWidth: 2)),
            );
          }
          if (snapshot.hasError || !snapshot.hasData) {
            return SizedBox(
              height: widget.mini ? 20 : 200,
              child: const Center(child: Icon(Icons.broken_image, color: Colors.grey)),
            );
          }
          final imageData = snapshot.data!;
          return GestureDetector(
            onTap: () {
              if (!widget.mini) _showImagePreviewModal(context, imageData);
            },
            child: ClipRRect(
              borderRadius: widget.mini
                  ? BorderRadius.circular(2.0)
                  : BorderRadius.circular(8.0),
              child: SizedBox(
                height: widget.mini ? 20 : 200,
                child: Image.memory(
                  imageData,
                  height: widget.mini ? 20 : 200,
                  fit: BoxFit.cover,
                  cacheWidth: 200,
                  gaplessPlayback: true,
                ),
              ),
            ),
          );
        },
      ),
    );
  }

  void _showImagePreviewModal(BuildContext context, Uint8List imageData) {
    showDialog(
      context: context,
      builder: (BuildContext context) {
        return Dialog(
          insetPadding: EdgeInsets.zero,
          backgroundColor: Colors.transparent,
          child: IntrinsicWidth(
            child: Column(
              mainAxisSize: MainAxisSize.min,
              crossAxisAlignment: CrossAxisAlignment.stretch,
              children: [
                Container(
                  color: Colors.black.withOpacity(0.7),
                  padding: const EdgeInsets.symmetric(vertical: 8, horizontal: 8),
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
                            icon: const Icon(Icons.download, color: Colors.white),
                            onPressed: () async {
                              await FileSaver.instance.saveFile(
                                name: 'image_${DateTime.now().millisecondsSinceEpoch}.jpg',
                                bytes: imageData,
                              );
                              ScaffoldMessenger.of(context).showSnackBar(
                                const SnackBar(
                                  content: Text('Image saved to download folder'),
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
                                [XFile.fromData(imageData)],
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
                      imageData,
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
