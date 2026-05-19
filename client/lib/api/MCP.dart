import 'dart:convert';
import 'package:code_highlight_view/themes/foundation.dart';
import 'package:http/http.dart' as http;
import 'package:flutter/foundation.dart';
import '../auth/auth-service.dart';
import '../utils/types.dart';
import 'package:path_provider/path_provider.dart';
import './stt.dart';
import 'package:flutter/material.dart';
import 'dart:io';
import './process.dart';
import 'package:path/path.dart' as path;

class MCPServerManager {
  final Map<String, ProcessManager> _servers = {};
  final Map<String, MCPServer> _statefulConfigs = {};
  final Map<String, Map<String, ProcessManager>> _convServers = {};

  Future<bool> startServer(MCPServer server) async {
    if (server.stateful) {
      _statefulConfigs[server.name] = server;
      return true;
    }

    if (_servers.containsKey(server.name)) {
      return false;
    }

    final processManager = ProcessManager(
      command: server.command,
      name: server.name,
      args: server.args,
    );

    final success = await processManager.start();
    if (success) {
      _servers[server.name] = processManager;
    }

    return success;
  }

  /// Returns true if [serverName] is a stateful MCP server.
  bool isStateful(String serverName) => _statefulConfigs.containsKey(serverName);

  /// Get or create a per-conversation process for a stateful MCP server.
  Future<ProcessManager> _getConversationServer(
    String conversationId,
    String serverName,
  ) async {
    _convServers[conversationId] ??= {};
    if (_convServers[conversationId]!.containsKey(serverName)) {
      return _convServers[conversationId]![serverName]!;
    }

    final config = _statefulConfigs[serverName]!;
    final pm = ProcessManager(
      command: config.command,
      name: serverName,
      args: config.args,
    );
    final success = await pm.start();
    if (!success) {
      throw Exception('Failed to start stateful MCP server $serverName');
    }

    // MCP initialize handshake.
    await pm.sendRequest('initialize', {
      'protocolVersion': '2024-11-05',
      'capabilities': {},
      'clientInfo': {'name': 'plurality-client', 'version': '1.0'},
    });
    try {
      await pm.sendRequest('notifications/initialized', {});
    } catch (_) {}

    _convServers[conversationId]![serverName] = pm;
    return pm;
  }

  Future<dynamic> sendRequest(
    String serverName,
    String method, [
    dynamic params,
    String? conversationId,
  ]) async {
    if (params == null) {
      params = {};
    }

    print(
      "MCPServerManager: sendRequest() called with serverName: $serverName, method: $method",
    );

    // Route stateful servers to per-conversation processes.
    if (_statefulConfigs.containsKey(serverName) && conversationId != null) {
      final pm = await _getConversationServer(conversationId, serverName);
      return pm.sendRequest(method, params);
    }

    final server = _servers[serverName];
    if (server == null) {
      throw Exception("Server '$serverName' is not running");
    }

    return server.sendRequest(method, params);
  }

  Stream<String>? getServerOutput(String serverName) {
    return _servers[serverName]?.outputStream;
  }

  Future<void> stopServer(String serverName) async {
    final server = _servers.remove(serverName);
    if (server != null) {
      await server.stop();
    }
  }

  /// Stop all stateful MCP processes for a conversation.
  Future<void> stopConversation(String conversationId) async {
    final servers = _convServers.remove(conversationId);
    if (servers != null) {
      for (final pm in servers.values) {
        await pm.stop();
      }
    }
  }

  Future<void> stopAll() async {
    final servers = List<ProcessManager>.from(_servers.values);
    _servers.clear();
    _statefulConfigs.clear();

    for (final server in servers) {
      await server.stop();
    }

    for (final convMap in _convServers.values) {
      for (final pm in convMap.values) {
        await pm.stop();
      }
    }
    _convServers.clear();
  }
}

class MCPServer {
  String command;
  String name;
  List<String> args;
  bool stateful;
  String description;
  String toolList = "";

  MCPServer({required this.command, required this.name, required this.args, this.stateful = false, this.description = ''});
}

class MCPService {
  static final MCPService _instance = MCPService._internal();
  static MCPService get instance => _instance;
  static Map<String, MCPServer> mcpServers = {};
  final serverManager = MCPServerManager();
  List<dynamic> toolLists = [];
  Map<String, String> toolServerNames = {};

  // Factory constructor to return the same instance every time
  factory MCPService() {
    return _instance;
  }

  // Private constructor used by the factory constructor
  MCPService._internal();

  Future<File> getMCPPath() async {
    final appDocumentDirectory = await getApplicationSupportDirectory();
    final mcpFile = File(path.join(appDocumentDirectory.path, 'mcp.json'));
    return mcpFile;
  }

  void initMCP() async {
    if (kIsWeb) {
      return;
    }
    if (Platform.isWindows || Platform.isLinux || Platform.isMacOS) {
      serverManager.stopAll();
      mcpServers = {};
      toolLists = [];
      toolServerNames = {};

      // read MCP file
      final mcpFile = await getMCPPath();

      print(
        'MCPService: initMCP() called on ${Platform.operatingSystem} with file: ${mcpFile.path}',
      );

      if (await mcpFile.exists()) {
        final mcpContent = await mcpFile.readAsString();
        final mcpData = jsonDecode(mcpContent);
        Map<String, dynamic> mcpServersReading = Map<String, dynamic>.from(
          mcpData['mcpServers'] ?? {},
        );

        // for each
        for (var entry in mcpServersReading.entries) {
          final name = entry.key;
          final server = entry.value;
          final command = server['command'] ?? '';
          final args = List<String>.from(server['args'] ?? []);
          final stateful = server['stateful'] == true;
          final description = server['description'] as String? ?? '';

          if (command.isEmpty || name.isEmpty) {
            print('Invalid server entry: $server');
            continue;
          }

          print("adding server: $name, command: $command, args: $args, stateful: $stateful");

          MCPServer mcpServer = MCPServer(
            command: command,
            name: name,
            args: args,
            stateful: stateful,
            description: description,
          );

          mcpServers[name] = mcpServer;

          // For stateful servers we start a temporary process just for tool
          // discovery, then stop it. Per-conversation processes are spawned
          // lazily by MCPServerManager.
          ProcessManager? discoveryPm;
          if (stateful) {
            discoveryPm = ProcessManager(command: command, name: name, args: args);
            final ok = await discoveryPm.start();
            if (!ok) {
              print('Failed to start discovery process for stateful server $name.');
              continue;
            }
            try {
              await discoveryPm.sendRequest('initialize', {
                'protocolVersion': '2024-11-05',
                'capabilities': {},
                'clientInfo': {'name': 'plurality-client', 'version': '1.0'},
              });
              await discoveryPm.sendRequest('notifications/initialized', {});
            } catch (_) {}
          }

          // Register with the manager (stateful configs are stored, not started).
          final success = await serverManager.startServer(mcpServer);
          if (!stateful && !success) {
            print('Failed to start server $name.');
            continue;
          }

          // Discover tools via tools/list.
          try {
            Map<String, dynamic>? response;
            if (stateful && discoveryPm != null) {
              response = await discoveryPm.sendRequest('tools/list', {})
                  as Map<String, dynamic>?;
            } else {
              response = await serverManager.sendRequest(name, 'tools/list')
                  as Map<String, dynamic>?;
            }

            if (response != null) {
              var t = response['tools'] ?? [];
              for (var i = 0; i < t.length; i++) {
                var tool = t[i];
                if (tool['inputSchema'] != null) {
                  var inputSchema = tool['inputSchema'];
                  if (inputSchema is Map<String, dynamic>) {
                    tool['parameters'] = inputSchema;
                    tool.remove('inputSchema');
                  }
                }
                final nsName = '${name}__${tool["name"]}';
              toolServerNames[nsName] = name;
              tool["name"] = nsName;
              tool["description"] = "[MCP server: $name] ${tool["description"] ?? ''}";
              }
              toolLists.addAll(t);
            } else {
              print('No response from $name.');
            }
          } catch (e) {
            print('Tool discovery failed for $name: $e');
          }

          // Stop the temporary discovery process for stateful servers.
          if (stateful && discoveryPm != null) {
            await discoveryPm.stop();
          }
        }

        // start servers
      } else {
        // create file with {}
        await mcpFile.create(recursive: true);
        await mcpFile.writeAsString('{}');
        print('mcp.json file created at ${mcpFile.path}');
      }
    }
  }

  List<Map<String, dynamic>> getToolList() {
    return List<Map<String, dynamic>>.from(toolLists);
  }

  /// Returns the server name for a (namespaced) tool name.
  String? getToolServerName(String toolName) {
    return toolServerNames[toolName];
  }

  /// Strips the namespace prefix from a namespaced tool name,
  /// returning the bare name the MCP process expects.
  static String bareToolName(String toolName) {
    final idx = toolName.indexOf('__');
    if (idx < 0) return toolName;
    return toolName.substring(idx + 2);
  }

  getToolsForServer(String serverName) {
    return toolLists
        .where((tool) => toolServerNames[tool["name"]] == serverName)
        .toList();
  }

  /// Returns the user-configured description for a server, or empty string.
  String getServerDescription(String serverName) {
    return mcpServers[serverName]?.description ?? '';
  }

  /// Returns all server descriptions keyed by server name.
  Map<String, String> getServerDescriptions() {
    final out = <String, String>{};
    for (final entry in mcpServers.entries) {
      if (entry.value.description.isNotEmpty) {
        out[entry.key] = entry.value.description;
      }
    }
    return out;
  }
}
