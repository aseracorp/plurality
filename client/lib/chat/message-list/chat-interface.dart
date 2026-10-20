void _onStatusChanged() {
    if (!mounted) return;
    final status = _chatService.conversationStatuses.value[widget.conversationId];
    final isProcessing = status?.isProcessing ?? false;
    final statusKey = status == null
        ? ''
        : '${status.state}|${status.activity}|${status.toolName}';
    final keyChanged = statusKey != _lastStatusKey;
    final processingChanged = isProcessing != _lastStatusProcessing;
    if (!keyChanged && !processingChanged) return;
    _lastStatusKey = statusKey;
    _lastStatusProcessing = isProcessing;
    setState(() {});
  }