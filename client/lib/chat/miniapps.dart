import 'package:flutter/material.dart';
import 'package:cached_network_image/cached_network_image.dart';
import '../utils/types.dart';
import '../api/mini-apps.dart';
import 'dart:convert';

class MiniAppsBrowser extends StatefulWidget {
  final Function(MiniApp) onStartMiniApp;
  final bool showPinnedOnly;

  const MiniAppsBrowser({
    Key? key,
    required this.onStartMiniApp,
    this.showPinnedOnly = false,
  }) : super(key: key);

  @override
  _MiniAppsBrowserState createState() => _MiniAppsBrowserState();
}

class _MiniAppsBrowserState extends State<MiniAppsBrowser> {
  final MiniAppsService _miniAppsService = MiniAppsService();
  List<MiniApp> _miniApps = [];
  bool _isLoading = true;
  String? _errorMessage;

  @override
  void initState() {
    super.initState();
    _loadMiniApps();
  }

  Future<void> _loadMiniApps() async {
    setState(() {
      _isLoading = true;
      _errorMessage = null;
    });

    try {
      if (widget.showPinnedOnly) {
        _miniApps = await _miniAppsService.getUserPinnedMiniApps();
      } else {
        _miniApps = await _miniAppsService.getAllMiniApps();
      }
    } catch (e) {
      setState(() {
        _errorMessage = e.toString();
      });
    } finally {
      setState(() {
        _isLoading = false;
      });
    }
  }

  void _showMiniAppDetails(BuildContext context, MiniApp miniApp) {
    showModalBottomSheet(
      context: context,
      isScrollControlled: true,
      shape: const RoundedRectangleBorder(
        borderRadius: BorderRadius.vertical(top: Radius.circular(20)),
      ),
      builder:
          (context) => _MiniAppDetailsModal(
            miniApp: miniApp,
            onStart: () {
              Navigator.pop(context);
              widget.onStartMiniApp(miniApp);
            },
            onPin: () async {
              try {
                await _miniAppsService.pinMiniApp(miniApp.id);
                ScaffoldMessenger.of(context).showSnackBar(
                  SnackBar(content: Text('${miniApp.name} added to favorites')),
                );
              } catch (e) {
                ScaffoldMessenger.of(context).showSnackBar(
                  SnackBar(
                    content: Text(
                      'Failed to add to favorites: ${e.toString()}',
                    ),
                  ),
                );
              }
            },
            onUnpin: () async {
              try {
                await _miniAppsService.unpinMiniApp(miniApp.id);
                ScaffoldMessenger.of(context).showSnackBar(
                  SnackBar(
                    content: Text('${miniApp.name} removed from favorites'),
                  ),
                );
                if (widget.showPinnedOnly) {
                  Navigator.pop(context);
                  _loadMiniApps();
                }
              } catch (e) {
                ScaffoldMessenger.of(context).showSnackBar(
                  SnackBar(
                    content: Text(
                      'Failed to remove from favorites: ${e.toString()}',
                    ),
                  ),
                );
              }
            },
            isPinned: widget.showPinnedOnly,
          ),
    );
  }

  @override
  Widget build(BuildContext context) {
    if (_isLoading) {
      return const Center(child: CircularProgressIndicator());
    }

    if (_errorMessage != null) {
      return Center(
        child: Column(
          mainAxisAlignment: MainAxisAlignment.center,
          children: [
            Text(
              'Error loading mini-apps',
              style: Theme.of(context).textTheme.titleMedium,
            ),
            const SizedBox(height: 8),
            Text(
              _errorMessage!,
              style: Theme.of(context).textTheme.bodySmall,
              textAlign: TextAlign.center,
            ),
            const SizedBox(height: 16),
            ElevatedButton(
              onPressed: _loadMiniApps,
              child: const Text('Retry'),
            ),
          ],
        ),
      );
    }

    if (_miniApps.isEmpty) {
      return Center(
        child: Column(
          mainAxisAlignment: MainAxisAlignment.center,
          children: [
            Icon(
              widget.showPinnedOnly ? Icons.star_border : Icons.apps_outlined,
              size: 64,
              color: Colors.grey,
            ),
            const SizedBox(height: 16),
            Text(
              widget.showPinnedOnly
                  ? 'No pinned mini-apps yet'
                  : 'No mini-apps available',
              style: Theme.of(context).textTheme.titleMedium,
            ),
            if (widget.showPinnedOnly) ...[
              const SizedBox(height: 16),
              ElevatedButton(
                onPressed: () {
                  // Navigate to all mini-apps or show all mini-apps
                  // This would depend on your app's navigation structure
                },
                child: const Text('Browse All Mini-Apps'),
              ),
            ],
          ],
        ),
      );
    }
    return RefreshIndicator(
      onRefresh: _loadMiniApps,
      child: ConstrainedBox(
        constraints: BoxConstraints(maxWidth: 600),
        child: GridView.builder(
          padding: const EdgeInsets.all(16),
          shrinkWrap:
              true, // This will make the grid take only the space it needs
          gridDelegate: const SliverGridDelegateWithFixedCrossAxisCount(
            crossAxisCount: 2,
            childAspectRatio: 1,
            crossAxisSpacing: 16,
            mainAxisSpacing: 16,
          ),
          itemCount: _miniApps.length,
          itemBuilder: (context, index) {
            final miniApp = _miniApps[index];
            return _MiniAppCard(
              miniApp: miniApp,
              onTap: () => _showMiniAppDetails(context, miniApp),
            );
          },
        ),
      ),
    );
  }
}

class _MiniAppCard extends StatelessWidget {
  final MiniApp miniApp;
  final VoidCallback onTap;

  const _MiniAppCard({Key? key, required this.miniApp, required this.onTap})
    : super(key: key);

  @override
  Widget build(BuildContext context) {
    return Card(
      elevation: 4,
      shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(16)),
      clipBehavior: Clip.antiAlias,
      child: InkWell(
        onTap: onTap,
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            Expanded(
              flex: 3,
              child: Container(
                color: Theme.of(context).colorScheme.surfaceVariant,
                child:
                    miniApp.iconURL.isNotEmpty
                        ? Image.memory(
                          base64Decode(miniApp.iconURL),
                          fit: BoxFit.cover,
                        )
                        : Icon(
                          Icons.extension,
                          size: 48,
                          color: Theme.of(context).colorScheme.primary,
                        ),
              ),
            ),
            Expanded(
              flex: 2,
              child: Padding(
                padding: const EdgeInsets.all(12.0),
                child: Column(
                  crossAxisAlignment: CrossAxisAlignment.start,
                  children: [
                    Text(
                      miniApp.name,
                      style: Theme.of(context).textTheme.titleMedium,
                      maxLines: 1,
                      overflow: TextOverflow.ellipsis,
                    ),
                    const SizedBox(height: 4),
                    Text(
                      miniApp.description,
                      style: Theme.of(context).textTheme.bodySmall,
                      maxLines: 2,
                      overflow: TextOverflow.ellipsis,
                    ),
                  ],
                ),
              ),
            ),
          ],
        ),
      ),
    );
  }
}

class _MiniAppDetailsModal extends StatelessWidget {
  final MiniApp miniApp;
  final VoidCallback onStart;
  final VoidCallback onPin;
  final VoidCallback onUnpin;
  final bool isPinned;

  const _MiniAppDetailsModal({
    Key? key,
    required this.miniApp,
    required this.onStart,
    required this.onPin,
    required this.onUnpin,
    this.isPinned = false,
  }) : super(key: key);

  @override
  Widget build(BuildContext context) {
    return Container(
      padding: const EdgeInsets.all(24),
      constraints: BoxConstraints(
        maxHeight: MediaQuery.of(context).size.height * 0.8,
      ),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Row(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Container(
                width: 80,
                height: 80,
                decoration: BoxDecoration(
                  color: Theme.of(context).colorScheme.surfaceVariant,
                  borderRadius: BorderRadius.circular(16),
                ),
                child:
                    miniApp.iconURL.isNotEmpty
                        ? ClipRRect(
                          borderRadius: BorderRadius.circular(16),
                          child: Image.memory(
                            base64Decode(miniApp.iconURL),
                            fit: BoxFit.cover,
                          ),
                        )
                        : Icon(
                          Icons.extension,
                          size: 36,
                          color: Theme.of(context).colorScheme.primary,
                        ),
              ),
              const SizedBox(width: 16),
              Expanded(
                child: Column(
                  crossAxisAlignment: CrossAxisAlignment.start,
                  children: [
                    Text(
                      miniApp.name,
                      style: Theme.of(context).textTheme.headlineSmall,
                    ),
                    const SizedBox(height: 4),
                    if (miniApp.author != null && miniApp.author != "")
                      Text(
                        'By ${miniApp.author}',
                        style: Theme.of(context).textTheme.bodyMedium?.copyWith(
                          color: Theme.of(context).colorScheme.onSurfaceVariant,
                        ),
                      ),
                    const SizedBox(height: 8),
                    Row(
                      children: [
                        IconButton(
                          icon: Icon(
                            isPinned ? Icons.star : Icons.star_border,
                            color: isPinned ? Colors.amber : null,
                          ),
                          onPressed: isPinned ? onUnpin : onPin,
                          tooltip:
                              isPinned
                                  ? 'Remove from favorites'
                                  : 'Add to favorites',
                        ),
                        const SizedBox(width: 8),
                        Text(
                          isPinned
                              ? 'Remove from favorites'
                              : 'Add to favorites',
                          style: Theme.of(context).textTheme.bodySmall,
                        ),
                      ],
                    ),
                  ],
                ),
              ),
            ],
          ),
          const SizedBox(height: 24),
          Text('Description', style: Theme.of(context).textTheme.titleMedium),
          const SizedBox(height: 8),
          Text(
            miniApp.description,
            style: Theme.of(context).textTheme.bodyMedium,
          ),
          const SizedBox(height: 24),
          if (miniApp.models.length > 1)
            if (miniApp.models.isNotEmpty) ...[
              Text('Models', style: Theme.of(context).textTheme.titleMedium),
              const SizedBox(height: 8),
              Wrap(
                spacing: 8,
                runSpacing: 8,
                children:
                    miniApp.models
                        .map(
                          (model) => Chip(
                            label: Text(model.name),
                            backgroundColor:
                                Theme.of(context).colorScheme.surfaceVariant,
                          ),
                        )
                        .toList(),
              ),
              const SizedBox(height: 24),
            ],
          if (miniApp.inputs.isNotEmpty) ...[
            Text('Input Types', style: Theme.of(context).textTheme.titleMedium),
            const SizedBox(height: 8),
            Expanded(
              child: ListView.builder(
                shrinkWrap: true,
                itemCount: miniApp.inputs.length,
                itemBuilder: (context, index) {
                  final input = miniApp.inputs[index];
                  return ListTile(
                    title: Text(input.name),
                    subtitle: Text(input.description),
                    leading: Icon(_getIconForInputType(input.type)),
                  );
                },
              ),
            ),
            const SizedBox(height: 16),
          ],
          SizedBox(
            width: double.infinity,
            child: ElevatedButton(
              onPressed: onStart,
              child: const Text('Start Conversation'),
            ),
          ),
        ],
      ),
    );
  }

  IconData _getIconForInputType(String type) {
    switch (type.toLowerCase()) {
      case 'text':
        return Icons.text_fields;
      case 'textarea':
        return Icons.subject;
      case 'dropdown':
        return Icons.arrow_drop_down_circle;
      case 'checkbox':
        return Icons.check_box;
      case 'radio':
        return Icons.radio_button_checked;
      case 'slider':
        return Icons.linear_scale;
      case 'date':
        return Icons.calendar_today;
      case 'time':
        return Icons.access_time;
      case 'file':
        return Icons.attach_file;
      case 'image':
        return Icons.image;
      default:
        return Icons.input;
    }
  }
}
