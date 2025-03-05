import 'package:flutter/gestures.dart';
import 'package:flutter/material.dart';
import 'dart:async';

class MiddleClickScroller extends StatefulWidget {
  /// The main content widget to display
  final Widget child;

  /// ScrollController for the main content
  final ScrollController scrollController;

  /// Icon color for the scroll indicator
  final Color iconColor;

  /// Size of the scroll indicator
  final double iconSize;

  /// Maximum scroll speed in pixels per frame
  final double maxScrollSpeed;

  const MiddleClickScroller({
    Key? key,
    required this.child,
    required this.scrollController,
    this.iconColor = Colors.blue,
    this.iconSize = 40,
    this.maxScrollSpeed = 200.0,
  }) : super(key: key);

  @override
  State<MiddleClickScroller> createState() => _MiddleClickScrollerState();
}

class _MiddleClickScrollerState extends State<MiddleClickScroller> {
  // Track if middle click scroll mode is active
  bool _isScrollModeActive = false;

  // Position for scroll icon
  double _iconPositionX = 0;
  double _iconPositionY = 0;

  // Initial activation position (anchor point)
  double _anchorPositionY = 0;

  // Current scroll speed
  double _currentScrollSpeed = 0;

  // Timer for continuous scrolling
  Timer? _scrollTimer;

  // Focus node to capture events
  late FocusNode _focusNode;

  @override
  void initState() {
    super.initState();
    _focusNode = FocusNode();
  }

  @override
  void dispose() {
    _scrollTimer?.cancel();
    _focusNode.dispose();
    super.dispose();
  }

  // Handle any mouse button click - toggle scroll mode
  void _handlePointerDown(PointerDownEvent event) {
    if (!_isScrollModeActive) {
      // Activate on middle mouse button only
      if (event.buttons == kMiddleMouseButton) {
        setState(() {
          _isScrollModeActive = true;
          _iconPositionX = event.localPosition.dx;
          _iconPositionY = event.localPosition.dy;
          _anchorPositionY = event.localPosition.dy;
          _currentScrollSpeed = 0;
        });
        _focusNode.requestFocus();

        // Start continuous scrolling timer
        _startScrollTimer();
      }
    } else {
      // Deactivate on any mouse button click when in scroll mode
      _stopScrolling();
    }
  }

  // Start the continuous scrolling timer
  void _startScrollTimer() {
    _scrollTimer?.cancel();
    _scrollTimer = Timer.periodic(Duration(milliseconds: 13), (timer) {
      if (!_isScrollModeActive || !mounted) {
        timer.cancel();
        return;
      }

      // Apply current scroll speed
      if (_currentScrollSpeed != 0 && widget.scrollController.hasClients) {
        final double currentOffset = widget.scrollController.offset;
        final double newOffset = (currentOffset + _currentScrollSpeed).clamp(
          0.0,
          widget.scrollController.position.maxScrollExtent,
        );

        widget.scrollController.jumpTo(newOffset);
      }
    });
  }

  // Stop scrolling and reset state
  void _stopScrolling() {
    _scrollTimer?.cancel();
    _scrollTimer = null;
    setState(() {
      _isScrollModeActive = false;
      _currentScrollSpeed = 0;
    });
  }

  // Handle mouse movement to update scroll speed
  void _handlePointerHover(PointerHoverEvent event) {
    if (!_isScrollModeActive) return;

    // Update icon position to follow cursor
    /*setState(() {
      _iconPositionX = event.localPosition.dx;
      _iconPositionY = event.localPosition.dy;
    });*/

    // Calculate distance from anchor point
    final double distance = event.localPosition.dy - _anchorPositionY;

    // Create a small dead zone around the anchor point
    if (distance.abs() < 5.0) {
      setState(() {
        _currentScrollSpeed = 0;
      });
      return;
    }

    // Calculate normalized scroll speed based on distance from anchor
    // The further from anchor, the faster it scrolls, but with a maximum speed
    double speed = distance * 0.2; // Base sensitivity

    // Cap the maximum speed
    if (speed > 0) {
      speed = speed.clamp(0, widget.maxScrollSpeed);
    } else {
      speed = speed.clamp(-widget.maxScrollSpeed, 0);
    }

    // Update current scroll speed - not inverting direction
    setState(() {
      _currentScrollSpeed = speed;
    });
  }

  @override
  Widget build(BuildContext context) {
    return Focus(
      focusNode: _focusNode,
      child: Listener(
        onPointerDown: _handlePointerDown,
        onPointerHover: _handlePointerHover,
        child: Stack(
          children: [
            // Main content
            widget.child,

            // Scroll indicator
            if (_isScrollModeActive)
              Positioned(
                left: _iconPositionX - (widget.iconSize / 2),
                top: _iconPositionY - (widget.iconSize / 2),
                child: IgnorePointer(
                  child: Container(
                    width: widget.iconSize,
                    height: widget.iconSize,
                    decoration: BoxDecoration(
                      color: Colors.black45,
                      shape: BoxShape.circle,
                    ),
                    child: Column(
                      mainAxisAlignment: MainAxisAlignment.center,
                      children: [
                        Icon(
                          Icons.swap_vert,
                          color: widget.iconColor,
                          size: widget.iconSize * 0.6,
                        ),
                        // Optional: Show scroll speed indicator
                        // if (_currentScrollSpeed != 0)
                        //   Icon(
                        //     _currentScrollSpeed > 0
                        //         ? Icons.arrow_downward
                        //         : Icons.arrow_upward,
                        //     color: widget.iconColor,
                        //     size: widget.iconSize * 0.3,
                        //   ),
                      ],
                    ),
                  ),
                ),
              ),
          ],
        ),
      ),
    );
  }
}
