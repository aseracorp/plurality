import 'dart:convert';

import '../utils/types.dart';

/// Strict-validate args for a client-side tool call against its declared
/// schema. Returns null when valid, or an LLM-facing error string.
///
/// Extracted from `ChatService._validateClientToolArgs` so the device channel
/// (docs/device-channel.md) can validate against the schemas IT advertised
/// without duplicating the rules. The body is intentionally identical to the
/// original inline version: chat-path validation must keep producing byte for
/// byte the same error text, because that text is what the model reads and
/// acts on ("unknown parameter(s) [...]", "missing required parameter(s)
/// [...]", "allowed parameters: [...]").
///
/// Why the check exists at all: the LLM can emit a parameter the tool never
/// declared (a hallucinated `op`, a renamed `path`). Handing that to
/// `ShellService.executeShellExec` / `FilesystemService.executeFsWrite` means
/// the executor's own defaults decide what happens — for a write tool that is
/// a mutation nobody asked for. Refusing before dispatch is the cheaper,
/// honest answer, and cheaper than the outer bound it would otherwise burn on
/// a call that cannot succeed.
String? validateClientToolArgs(
  ToolCall toolCall,
  List<Map<String, dynamic>> defs,
) {
  Map<String, dynamic>? def;
  for (final d in defs) {
    if (d['name'] == toolCall.function.name) {
      def = d;
      break;
    }
  }
  if (def == null) return null;
  final params = (def['parameters'] ?? def['input_schema']) as Map?;
  if (params == null) return null;

  final propsRaw = params['properties'];
  final allowed =
      propsRaw is Map ? propsRaw.keys.map((k) => k.toString()).toSet() : <String>{};
  final requiredRaw = params['required'];
  final required = requiredRaw is List
      ? requiredRaw.map((e) => e.toString()).toList()
      : <String>[];

  final argsStr =
      toolCall.function.arguments.isEmpty ? '{}' : toolCall.function.arguments;
  dynamic decoded;
  try {
    decoded = jsonDecode(argsStr);
  } catch (e) {
    return 'Error: invalid arguments for ${toolCall.function.name}: arguments must be a JSON object ($e)';
  }
  if (decoded is! Map) {
    return 'Error: invalid arguments for ${toolCall.function.name}: arguments must be a JSON object';
  }
  final provided = decoded.keys.map((k) => k.toString()).toSet();

  final unknown = provided.difference(allowed).toList()..sort();
  final missing = required.where((r) => !provided.contains(r)).toList()..sort();
  if (unknown.isEmpty && missing.isEmpty) return null;

  final allowedSorted = allowed.toList()..sort();
  final parts = <String>[];
  if (unknown.isNotEmpty) parts.add('unknown parameter(s) [${unknown.join(', ')}]');
  if (missing.isNotEmpty) parts.add('missing required parameter(s) [${missing.join(', ')}]');
  return 'Error: invalid arguments for ${toolCall.function.name}: ${parts.join('; ')}; allowed parameters: [${allowedSorted.join(', ')}]';
}
