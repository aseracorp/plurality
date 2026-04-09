import 'dart:convert';
import 'package:code_highlight_view/themes/foundation.dart';
import 'package:http/http.dart' as http;
import 'package:flutter/foundation.dart';
import '../auth/auth-service.dart';
import '../utils/types.dart';
import './balance.dart';
import 'package:path_provider/path_provider.dart';
import './stt.dart';
import 'package:flutter/material.dart';
import 'dart:io';
import './process.dart';
import 'package:path/path.dart' as path;

class MCPServerManager {
  final Map<String, ProcessManager> _servers = {};

  Future<bool> startServer(MCPServer server) async {
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

  Future<dynamic> sendRequest(
    String serverName,
    String method, [
    dynamic params,
  ]) async {
    if (params == null) {
      params = {};
    }

    print(
      "MCPServerManager: sendRequest() called with serverName: $serverName, method: $method, params: $params",
    );
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

  Future<void> stopAll() async {
    final servers = List<ProcessManager>.from(_servers.values);
    _servers.clear();

    for (final server in servers) {
      await server.stop();
    }
  }
}

class MCPServer {
  String command;
  String name;
  List<String> args;
  String toolList = "";

  MCPServer({required this.command, required this.name, required this.args});
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

          if (command.isEmpty || name.isEmpty) {
            print('Invalid server entry: $server');
            continue;
          }

          print("adding server: $name, command: $command, args: $args");

          // add to list of servers
          MCPServer mcpServer = MCPServer(
            command: command,
            name: name,
            args: args,
          );
          // do something with mcpServer

          mcpServers[name] = mcpServer;

          // start server
          final success = await serverManager.startServer(mcpServer);

          if (success) {
            print('Server $name started successfully.');
          } else {
            print('Failed to start server $name.');
          }

          // Send tools/list
          final response =
              await serverManager.sendRequest(name, 'tools/list')
                  as Map<String, dynamic>?;

          if (response != null) {
            var t = response['tools'] ?? [];
            // for each tool replace inputSchema with Parameters
            for (var i = 0; i < t.length; i++) {
              var tool = t[i];
              if (tool['inputSchema'] != null) {
                var inputSchema = tool['inputSchema'];
                if (inputSchema is Map<String, dynamic>) {
                  tool['parameters'] = inputSchema;
                  tool.remove('inputSchema'); // Use remove instead of delete
                }
              }
              toolServerNames[tool["name"]] = name;
            }

            toolLists.addAll(t);
          } else {
            print('No response from $name.');
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

  String? getToolServerName(String toolName) {
    return toolServerNames[toolName];
  }

  getToolsForServer(String serverName) {
    return toolLists
        .where((tool) => toolServerNames[tool["name"]] == serverName)
        .toList();
  }
}
