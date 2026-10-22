  /// Anchor the main list to its last item. Uses the itemCount from the most
  /// recent build so it matches whatever super_sliver_list is rendering, and
  /// only jumps when the target is actually within range — jumping past the
  /// current itemCount (which can shrink between the build and this callback
  /// when streaming content toggles) throws an out-of-bounds assertion in
  /// super_sliver_list and crashes the whole tab.
  void _scrollToBottom() {
    if (!_listController.isAttached ||
        !_mainScrollController.hasClients ||
        _lastMainItemCount <= 0) return;
    final target = _lastMainItemCount - 1;
    // Guard against jumping to an index that no longer exists. When the
    // visible/itemCount changed between the last build and this callback
    // (e.g. an SSE 'done' removed the streaming slot), jumping past the end
    // throws in super_sliver_list — the crash you'd see when opening an
    // active conversation. Clamp to the last valid index instead.
    final safeTarget = target.clamp(0, _lastMainItemCount - 1);
    _listController.jumpToItem(
      index: safeTarget,
      scrollController: _mainScrollController,
      alignment: 1.0,
    );
  }