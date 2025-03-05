import 'package:flutter/material.dart';

class MiniMap extends StatefulWidget {
  /// The main content widget to display
  final Widget child;

  /// ScrollController for the main content
  final ScrollController mainScrollController;

  /// ScrollController for the minimap content
  final ScrollController miniMapScrollController;

  /// Widget to display in the minimap
  final Widget miniMapContent;

  /// Width of the minimap sidebar
  final double miniMapWidth;

  /// Color of the overlay
  final Color overlayColor;

  /// Fixed height of the overlay
  final double overlayHeight;

  final bool enabled;

  const MiniMap({
    Key? key,
    required this.child,
    required this.mainScrollController,
    required this.miniMapScrollController,
    required this.miniMapContent,
    this.miniMapWidth = 140,
    this.overlayColor = Colors.blue,
    this.overlayHeight = 80,
    this.enabled = false,
  }) : super(key: key);

  @override
  State<MiniMap> createState() => _MiniMapState();
}

class _MiniMapState extends State<MiniMap> with WidgetsBindingObserver {
  // Track if we're currently handling a scroll update
  bool _isUpdatingScroll = false;

  // Overlay position
  double _overlayPosition = 0;

  // Whether we are currently dragging the overlay
  bool _isDraggingOverlay = false;

  // Track drag start position for overlay dragging
  double _dragStartOverlayPosition = 0;
  double _dragStartY = 0;

  // Track minimap size
  double _miniMapHeight = 0;
  final GlobalKey _miniMapKey = GlobalKey();

  @override
  void initState() {
    super.initState();
    // Add listeners to synchronize scrolling
    widget.mainScrollController.addListener(_syncFromMainScroll);

    // Add observer for screen size changes
    WidgetsBinding.instance.addObserver(this);

    // Calculate initial overlay position
    WidgetsBinding.instance.addPostFrameCallback((_) {
      _updateMiniMapSize();
      _updateOverlayPosition();
    });
  }

  @override
  void didUpdateWidget(MiniMap oldWidget) {
    super.didUpdateWidget(oldWidget);

    // Update measurements when widget properties change
    WidgetsBinding.instance.addPostFrameCallback((_) {
      _updateMiniMapSize();
      _updateOverlayPosition();
    });
  }

  @override
  void didChangeMetrics() {
    // This is called when screen dimensions change (e.g., rotation, keyboard visibility)
    WidgetsBinding.instance.addPostFrameCallback((_) {
      _updateMiniMapSize();
      _updateOverlayPosition();
    });
  }

  @override
  void dispose() {
    // Remove listeners and observers
    widget.mainScrollController.removeListener(_syncFromMainScroll);
    WidgetsBinding.instance.removeObserver(this);
    super.dispose();
  }

  // Update the size of the minimap
  void _updateMiniMapSize() {
    if (!mounted) return;

    final RenderBox? renderBox =
        _miniMapKey.currentContext?.findRenderObject() as RenderBox?;
    if (renderBox != null) {
      setState(() {
        _miniMapHeight = renderBox.size.height;
      });
    }
  }

  // Sync minimap and overlay position when main content scrolls
  void _syncFromMainScroll() {
    if (_isUpdatingScroll || _isDraggingOverlay) return;

    _updateOverlayPosition();

    // Sync minimap scroll with main content
    if (widget.mainScrollController.hasClients &&
        widget.miniMapScrollController.hasClients &&
        widget.mainScrollController.position.maxScrollExtent > 0) {
      _isUpdatingScroll = true;

      // Calculate relative position
      final double scrollRatio =
          widget.mainScrollController.offset /
          widget.mainScrollController.position.maxScrollExtent;

      // Apply to minimap with jumpTo for immediate response
      widget.miniMapScrollController.jumpTo(
        scrollRatio * widget.miniMapScrollController.position.maxScrollExtent,
      );

      _isUpdatingScroll = false;
    }
  }

  // Update the overlay position based on main scroll position
  void _updateOverlayPosition() {
    if (!mounted) return;
    if (!widget.mainScrollController.hasClients) return;
    if (_miniMapHeight <= 0) {
      _updateMiniMapSize();
      return;
    }

    setState(() {
      if (widget.mainScrollController.position.maxScrollExtent > 0) {
        // Calculate scroll progress
        final double scrollProgress =
            widget.mainScrollController.offset /
            widget.mainScrollController.position.maxScrollExtent;

        // Calculate available space for the overlay to move using minimap height
        final double availableHeight = _miniMapHeight - widget.overlayHeight;

        // Set position
        _overlayPosition = scrollProgress * availableHeight;
      } else {
        _overlayPosition = 0;
      }
    });
  }

  // Handle tap on the minimap background (not on overlay)
  void _handleMiniMapTap(TapDownDetails details) {
    if (!widget.mainScrollController.hasClients) return;
    if (_miniMapHeight <= 0) return;

    final double tapPosition = details.localPosition.dy;

    // Check if tap is on the overlay
    final bool isOnOverlay =
        tapPosition >= _overlayPosition &&
        tapPosition <= _overlayPosition + widget.overlayHeight;

    if (!isOnOverlay) {
      // Calculate center position for the overlay
      final double targetCenter = tapPosition.clamp(
        widget.overlayHeight / 2,
        _miniMapHeight - (widget.overlayHeight / 2),
      );

      // Set new overlay position
      setState(() {
        _overlayPosition = targetCenter - (widget.overlayHeight / 2);
      });

      // Scroll based on new overlay position
      _scrollBasedOnOverlayPosition();
    }
  }

  // Start dragging the overlay
  void _handleOverlayDragStart(DragStartDetails details) {
    setState(() {
      _isDraggingOverlay = true;
      _dragStartOverlayPosition = _overlayPosition;
      _dragStartY = details.localPosition.dy;
    });
  }

  // Handle overlay dragging with better tracking to prevent gesture conflicts
  void _handleOverlayDragUpdate(DragUpdateDetails details) {
    if (!_isDraggingOverlay) return;
    if (_miniMapHeight <= 0) return;

    // Calculate new position based on drag
    final double dragDelta = details.localPosition.dy - _dragStartY;
    final double newPosition = (_dragStartOverlayPosition + dragDelta).clamp(
      0.0,
      _miniMapHeight - widget.overlayHeight,
    );

    // Only update if position has changed
    if (newPosition != _overlayPosition) {
      setState(() {
        _overlayPosition = newPosition;
      });

      // Scroll based on overlay position
      _scrollBasedOnOverlayPosition();
    }
  }

  // End dragging the overlay
  void _handleOverlayDragEnd(DragEndDetails details) {
    setState(() {
      _isDraggingOverlay = false;
    });
  }

  // Handle drag on the minimap background (not on overlay)
  void _handleMiniMapDragUpdate(DragUpdateDetails details) {
    if (_isDraggingOverlay) return;
    if (!widget.mainScrollController.hasClients) return;
    if (_miniMapHeight <= 0) return;

    final double tapPosition = details.localPosition.dy;

    // Only proceed if we're still within the valid height range
    if (tapPosition >= 0 && tapPosition <= _miniMapHeight) {
      // Calculate center position for the overlay
      final double targetPosition = (tapPosition - (widget.overlayHeight / 2))
          .clamp(0.0, _miniMapHeight - widget.overlayHeight);

      // Set new overlay position
      setState(() {
        _overlayPosition = targetPosition;
      });

      // Scroll based on overlay position
      _scrollBasedOnOverlayPosition();
    }
  }

  // Scroll both main content and minimap based on overlay position
  void _scrollBasedOnOverlayPosition() {
    if (!widget.mainScrollController.hasClients ||
        !widget.miniMapScrollController.hasClients ||
        _miniMapHeight <= 0)
      return;

    _isUpdatingScroll = true;

    final double maxPosition = _miniMapHeight - widget.overlayHeight;
    final double scrollPercent =
        maxPosition > 0 ? _overlayPosition / maxPosition : 0;

    // Calculate scroll positions
    final double mainScrollTarget =
        scrollPercent * widget.mainScrollController.position.maxScrollExtent;
    final double miniScrollTarget =
        scrollPercent * widget.miniMapScrollController.position.maxScrollExtent;

    // Update scroll positions with jumpTo for immediate response
    widget.mainScrollController.jumpTo(mainScrollTarget);
    widget.miniMapScrollController.jumpTo(miniScrollTarget);

    _isUpdatingScroll = false;
  }

  @override
  Widget build(BuildContext context) {
    if (!widget.enabled) {
      return widget.child;
    }

    // Using LayoutBuilder to get the available height
    return Row(
      children: [
        // Main content area
        Expanded(child: widget.child),

        // MiniMap sidebar
        LayoutBuilder(
          builder: (context, constraints) {
            // Update minimap height when layout changes
            WidgetsBinding.instance.addPostFrameCallback((_) {
              if (_miniMapHeight != constraints.maxHeight) {
                _updateMiniMapSize();
                _updateOverlayPosition();
              }
            });

            return SizedBox(
              width: widget.miniMapWidth,
              key: _miniMapKey,
              child: Stack(
                children: [
                  // MiniMap content
                  Container(
                    decoration: BoxDecoration(
                      color: Colors.white,
                      boxShadow: [
                        BoxShadow(
                          color: Colors.black.withOpacity(0.15),
                          blurRadius: 3,
                          offset: const Offset(-4, 0),
                        ),
                      ],
                    ),
                    child: ScrollConfiguration(
                      behavior: ScrollConfiguration.of(context).copyWith(
                        scrollbars: false, // Hide scrollbar
                      ),
                      child: widget.miniMapContent,
                    ),
                  ),

                  // Transparent overlay for background interaction
                  Positioned.fill(
                    child: GestureDetector(
                      onTapDown: _handleMiniMapTap,
                      onVerticalDragUpdate:
                          _isDraggingOverlay ? null : _handleMiniMapDragUpdate,
                      behavior: HitTestBehavior.translucent,
                      child: Container(color: Colors.transparent),
                    ),
                  ),

                  // Viewport indicator overlay - draggable
                  Positioned(
                    left: 4,
                    right: 4,
                    top: _overlayPosition,
                    height: widget.overlayHeight,
                    child: GestureDetector(
                      onVerticalDragStart: _handleOverlayDragStart,
                      onVerticalDragUpdate: _handleOverlayDragUpdate,
                      onVerticalDragEnd: _handleOverlayDragEnd,
                      behavior: HitTestBehavior.opaque,
                      child: Container(
                        decoration: BoxDecoration(
                          color: widget.overlayColor.withOpacity(0.2),
                          border: Border.all(
                            color: widget.overlayColor,
                            width: 2,
                          ),
                          borderRadius: BorderRadius.circular(4),
                        ),
                        // Optional: Add a handle or icon to indicate draggability
                        child: Center(
                          child: Icon(
                            Icons.drag_handle,
                            color: widget.overlayColor.withOpacity(0.6),
                            size: 20,
                          ),
                        ),
                      ),
                    ),
                  ),
                ],
              ),
            );
          },
        ),
      ],
    );
  }
}
