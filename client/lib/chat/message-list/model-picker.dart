import 'dart:io' show Platform, Process;

import 'package:flutter/foundation.dart' show kIsWeb;
import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:plurality/api/MCP.dart';
import '../../api/balance.dart';
import '../../utils/types.dart';

final List<String> VisionModelOptions = [
  // Fireworks Vision
  "llama4-maverick-instruct-basic",
  "qwen3p5-397b-a17b",
  "qwen3p6-plus",
  "kimi-k2p5",
  // OpenAI GPT-5 + GPT-4.1
  'ChatGPT/gpt-5.2',
  'ChatGPT/gpt-5.2-pro',
  'ChatGPT/gpt-5',
  'ChatGPT/gpt-5-mini',
  'ChatGPT/gpt-4.1',
  'ChatGPT/gpt-4.1-mini',
  // Claude 4.5
  'Claude/claude-haiku-4-6',
  'Claude/claude-sonnet-4-6',
  'Claude/claude-opus-4-6',
  // Gemini 2.5 + 3
  "Gemini/gemini-2.5-flash",
  "Gemini/gemini-2.5-flash-lite",
  "Gemini/gemini-2.5-pro",
  // "Gemini/gemini-3-flash",
  // "Gemini/gemini-3-pro", // not working
];

// TODO find a better one...

const modelPresentFastText = Model(
  name: 'Gemini/gemini-2.5-flash',
  params: null,
  tools: ['search_web', 'place_search', 'visit_link', 'generate_image', 'search_conversations', 'retrieve_conversation'],
);
const modelPresentFastVision = Model(
  name: 'Gemini/gemini-2.5-flash',
  params: null,
  tools: ['search_web', 'place_search', 'visit_link', 'generate_image', 'search_conversations', 'retrieve_conversation'],
);
const modelPresentFastImageGen = Model(
  name: 'black-forest-labs/FLUX.2-dev',
  params: null,
);

// Preset configurations
final Map<String, ModelSelected> modelPresets = {
  'Fast': ModelSelected(
    text: modelPresentFastText,
    vision: modelPresentFastVision,
    imageGen: modelPresentFastImageGen,
  ),
  'Balanced': ModelSelected(
    text: Model(
      name: 'qwen3p6-plus',
      params: null,
      tools: ['search_web', 'place_search', 'visit_link', 'generate_image', 'search_conversations', 'retrieve_conversation'],
    ),
    vision: Model(
      name: 'qwen3p6-plus',
      params: null,
      tools: ['search_web', 'place_search', 'visit_link', 'generate_image', 'search_conversations', 'retrieve_conversation'],
    ),
    imageGen: Model(name: 'black-forest-labs/FLUX.2-dev', params: null),
  ),
  'Smart': ModelSelected(
    text: Model(
      name: 'glm-5p1',
      params: null,
      tools: ['search_web', 'place_search', 'visit_link', 'generate_image', 'search_conversations', 'retrieve_conversation'],
    ),
    vision: Model(name: 'qwen3p6-plus', params: null, tools: ['generate_image', 'search_conversations', 'retrieve_conversation']),
    imageGen: Model(name: 'black-forest-labs/FLUX.2-pro', params: null),
  ),
};

class ModelSelectionModal extends ConsumerStatefulWidget {
  final Function(ModelSelected) onModelSelected;
  final ModelSelected selectedModel;

  const ModelSelectionModal({
    Key? key,
    required this.onModelSelected,
    required this.selectedModel,
  }) : super(key: key);

  @override
  ConsumerState<ModelSelectionModal> createState() =>
      ModelSelectionModalState();
}

class ModelSelectionModalState extends ConsumerState<ModelSelectionModal>
    with SingleTickerProviderStateMixin {
  // Available options for each dropdown
  final List<String> _modelOptions = [
    // Fireworks - Llama 4
    "llama4-maverick-instruct-basic",
    // Fireworks - DeepSeek
    'deepseek-r1',
    'deepseek-r1-basic',
    'deepseek-r1-0528',
    'deepseek-v3',
    'deepseek-v3-0324',
    'deepseek-v3p2',
    // Fireworks - Qwen & Kimi
    // "qwen3p5-397b-a17b",
    'qwen3p6-plus',
    'kimi-k2p5',
    'glm-5p1',
    'minimax-m2p5',
    // OpenAI GPT-5 + GPT-4.1
    'ChatGPT/gpt-5.2',
    'ChatGPT/gpt-5.2-pro',
    'ChatGPT/gpt-5',
    'ChatGPT/gpt-5-mini',
    'ChatGPT/gpt-5-nano',
    'ChatGPT/gpt-4.1',
    'ChatGPT/gpt-4.1-mini',
    'ChatGPT/gpt-4.1-nano',
    // Claude 4.5
    'Claude/claude-haiku-4-6',
    'Claude/claude-sonnet-4-6',
    'Claude/claude-opus-4-6',
    // Gemini 2.5 + 3
    "Gemini/gemini-2.5-flash",
    "Gemini/gemini-2.5-flash-lite",
    "Gemini/gemini-2.5-pro",
    // "Gemini/gemini-3-flash",
    // "Gemini/gemini-3-pro",
  ];

  final List<String> _imageGenModelOptions = [
    'black-forest-labs/FLUX.2-dev',
    'black-forest-labs/FLUX.2-pro',
  ];

  final List<Map<String, dynamic>> _baseFunctions = [
    {
      'key': 'search_web',
      'label': 'Search Web',
      'description': 'Search sites via Google',
      'enabled': true,
    },
    {
      'key': 'place_search',
      'label': 'Place Search',
      'description': 'Search locations via Google Maps',
      'enabled': true,
    },
    {
      'key': 'visit_link',
      'label': 'Visit Link',
      'description': 'Visit websites shared in the chat',
      'enabled': true,
    },
    {
      'key': 'roll_dice',
      'label': 'Roll Dice',
      'description': 'Well... rolls a dice',
      'enabled': true,
    },
    {
      'key': 'generate_image',
      'label': 'Image Generation',
      'description': 'Generate images from text descriptions',
      'enabled': true,
    },
    {
      'key': 'search_conversations',
      'label': 'Search Conversations',
      'description': 'Search past conversations by topic',
      'enabled': true,
    },
    {
      'key': 'retrieve_conversation',
      'label': 'Retrieve Conversation',
      'description': 'Retrieve messages from a past conversation',
      'enabled': true,
    },
  ];

  List<Map<String, dynamic>> _functions = [];

  initFunctions() {
    List<Map<String, dynamic>> finalList = [];
    finalList.addAll(_baseFunctions);

    var clientSide = MCPService().getToolList();

    if (clientSide.isEmpty) {
      return finalList;
    }

    for (var i = 0; i < clientSide.length; i++) {
      var tool = clientSide[i];
      var serverName = MCPService().getToolServerName(tool['name']);
      if (serverName == null || serverName.isEmpty) {
        continue;
      }
      var description = tool['description'] ?? 'No description available';
      if (description.length > 100) {
        description = description.substring(0, 100) + '...';
      }
      // only keep first line of description
      if (description.contains('\n')) {
        description = description.split('\n')[0];
      }

      if (finalList.any((element) => element['key'] == serverName)) {
        // If the server already exists, add the tool to its tools list
        finalList
            .firstWhere((element) => element['key'] == serverName)['tools']
            .add(tool['name']);
      } else {
        finalList.add({
          'key': serverName,
          'label': serverName,
          'description': description,
          'enabled': true,
          'tools': [tool['name']],
        });
      }
    }

    return finalList;
  }

  String _selectedModel = '';
  String _selectedVisionModel = '';
  String _selectedImageGenModel = '';

  late TabController _tabController;
  int _currentTabIndex = 0;

  static getFastPreset() => modelPresets['Fast']!;

  @override
  void initState() {
    super.initState();
    _tabController = TabController(
      length: 3,
      vsync: this,
    ); // Change length to 3
    _tabController.addListener(_handleTabChange);

    _functions = initFunctions();

    _selectedModel = widget.selectedModel.text?.name ?? _modelOptions.first;
    _selectedVisionModel =
        widget.selectedModel.vision?.name ?? VisionModelOptions.first;
    _selectedImageGenModel =
        widget.selectedModel.imageGen?.name ?? _imageGenModelOptions.first;

    if (widget.selectedModel.text?.tools != null) {
      for (var function in _functions) {
        if (function['tools'] == null) {
          function['enabled'] = widget.selectedModel.text!.tools!.contains(
            function['key'],
          );
        } else {
          // check each tool in the list is enabled
          for (var tool in function['tools']) {
            if (widget.selectedModel.text!.tools!.contains(tool)) {
              function['enabled'] = true;
            } else {
              function['enabled'] = false;
              break;
            }
          }
        }
      }
    }

    // Check if user is free and set default models if needed
    WidgetsBinding.instance.addPostFrameCallback((_) {
      _checkAndSetDefaultModels();
    });
  }

  String _getSelectedPresetName() {
    // Find which preset matches the current selection
    for (var entry in modelPresets.entries) {
      if (entry.value.text?.name == _selectedModel &&
          entry.value.vision?.name == _selectedVisionModel &&
          entry.value.imageGen?.name == _selectedImageGenModel) {
        return entry.key;
      }
    }

    // Default to Balanced if no match
    return '';
  }

  void _handleTabChange() {
    setState(() {
      _currentTabIndex = _tabController.index;
    });
  }

  void _checkAndSetDefaultModels() {
    final balanceState = ref.read(balanceProvider);
    final balance = balanceState.value;
    final isFree = balance?.planName == 'Free';

    if (isFree) {
      if (_selectedVisionModel != modelPresentFastVision.name ||
          _selectedImageGenModel != modelPresentFastImageGen.name ||
          _selectedModel != modelPresentFastText.name) {
        setState(() {
          _selectedModel = modelPresentFastText.name;
          _selectedVisionModel = modelPresentFastVision.name;
          _selectedImageGenModel = modelPresentFastImageGen.name;
        });

        // Call setModel to update the selected models
        widget.onModelSelected(
          ModelSelected(
            text: modelPresentFastText,
            vision: modelPresentFastVision,
            imageGen: modelPresentFastImageGen,
          ),
        );
      }
    }

    // Check if the selected models are still in the list
    if (!_modelOptions.contains(_selectedModel)) {
      setState(() {
        _selectedModel = _modelOptions.first;
      });
    }
    if (!VisionModelOptions.contains(_selectedVisionModel)) {
      setState(() {
        _selectedVisionModel = VisionModelOptions.first;
      });
    }
    if (!_imageGenModelOptions.contains(_selectedImageGenModel)) {
      setState(() {
        _selectedImageGenModel = _imageGenModelOptions.first;
      });
    }
  }

  @override
  void dispose() {
    _tabController.removeListener(_handleTabChange);
    _tabController.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return Dialog(
      shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(16.0)),
      elevation: 0,
      backgroundColor: Colors.transparent,
      child: contentBox(context),
    );
  }

  Widget contentBox(BuildContext context) {
    final balanceState = ref.watch(balanceProvider);
    final balance = balanceState.value;
    final isFree = balance?.planName == 'Free';
    final isDarkMode = Theme.of(context).brightness == Brightness.dark;

    return Container(
      constraints: const BoxConstraints(maxWidth: 400),
      padding: const EdgeInsets.all(20),
      decoration: BoxDecoration(
        shape: BoxShape.rectangle,
        color: Theme.of(context).scaffoldBackgroundColor,
        borderRadius: BorderRadius.circular(16),
        boxShadow: const [
          BoxShadow(
            color: Colors.black26,
            blurRadius: 10.0,
            offset: Offset(0.0, 10.0),
          ),
        ],
      ),
      child: SingleChildScrollView(
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: <Widget>[
            const Text(
              'Select AI Models',
              style: TextStyle(fontSize: 22, fontWeight: FontWeight.w600),
            ),
            const SizedBox(height: 16),

            // Tab bar
            TabBar(
              controller: _tabController,
              tabs: const [
                Tab(text: 'Presets'),
                Tab(text: 'Custom'),
                Tab(text: 'Functions'),
              ],
              labelColor: Theme.of(context).colorScheme.primary,
              unselectedLabelColor: isDarkMode ? Colors.white : Colors.black,
              indicatorColor: Theme.of(context).colorScheme.primary,
            ),

            const SizedBox(height: 24),

            // Tab content
            SizedBox(
              height: _currentTabIndex == 0 ? 300 : 300,
              child: TabBarView(
                controller: _tabController,
                children: [
                  // Presets tab
                  _buildPresetsTab(isFree),
                  // Custom tab
                  _buildCustomTab(isFree),
                  // Functions tab
                  _buildFunctionsTab(),
                ],
              ),
            ),

            // Free user message
            if (isFree)
              Container(
                margin: const EdgeInsets.only(top: 16),
                padding: const EdgeInsets.all(10),
                decoration: BoxDecoration(
                  color: Colors.red.shade100,
                  borderRadius: BorderRadius.circular(8),
                  border: Border.all(color: Colors.red),
                ),
                child: const Text(
                  "Only subscribers can change the models, free users are restricted to the currently selected models",
                  style: TextStyle(
                    color: Colors.red,
                    fontWeight: FontWeight.bold,
                  ),
                  textAlign: TextAlign.center,
                ),
              ),

            const SizedBox(height: 24),

            // Action Buttons
            Row(
              mainAxisAlignment: MainAxisAlignment.end,
              children: [
                if (!kIsWeb && (Platform.isWindows || Platform.isLinux || Platform.isMacOS))
                  TextButton(
                    onPressed: () async {
                      // Open the MCP file in the default editor
                      final appDocumentDirectory =
                          (await MCPService().getMCPPath()).path;
                      if (Platform.isWindows) {
                        Process.run('explorer', [appDocumentDirectory]);
                      } else if (Platform.isMacOS) {
                        Process.run('open', [appDocumentDirectory]);
                      } else if (Platform.isLinux) {
                        Process.run('xdg-open', [appDocumentDirectory]);
                      }
                    },
                    child: const Text('Edit MCP File (BETA)'),
                  ),
                TextButton(
                  onPressed: () {
                    Navigator.pop(context);
                  },
                  child: const Text('Cancel'),
                ),
                const SizedBox(width: 8),
                if (_currentTabIndex == 1 || _currentTabIndex == 2)
                  ElevatedButton(
                    onPressed:
                        isFree
                            ? null
                            : () {
                              // Get enabled tools
                              final enabledTools =
                                  _functions
                                      .where((function) => function['enabled'])
                                      .map((function) {
                                        final tools = function['tools'];
                                        if (tools != null && tools is List) {
                                          return tools
                                              .map((tool) => tool as String)
                                              .toList();
                                        } else {
                                          return [function['key'] as String];
                                        }
                                      })
                                      .expand((tool) => tool)
                                      .toSet() // Remove duplicates
                                      .toList();

                              // Return the selected models and tools to the parent widget
                              final selectedModels =
                                  _currentTabIndex == 0
                                      ? _getSelectedPreset()
                                      : ModelSelected(
                                        text: Model(
                                          name: _selectedModel,
                                          params: null,
                                          tools: enabledTools,
                                        ),
                                        vision: Model(
                                          name: _selectedVisionModel,
                                          params: null,
                                          tools: enabledTools,
                                        ),
                                        imageGen: Model(
                                          name: _selectedImageGenModel,
                                          params: null,
                                        ),
                                      );

                              widget.onModelSelected(selectedModels);
                              Navigator.pop(context);
                            },
                    style: ElevatedButton.styleFrom(
                      backgroundColor: Theme.of(context).primaryColor,
                      foregroundColor: Colors.white,
                      disabledBackgroundColor: Colors.grey.shade300,
                      disabledForegroundColor: Colors.grey.shade500,
                    ),
                    child: const Text('Confirm'),
                  ),
              ],
            ),
          ],
        ),
      ),
    );
  }

  Widget _buildPresetsTab(bool isFree) {
    return Column(
      children: [
        _buildPresetButton(
          'Fast',
          'Fast and low cost',
          '\$',
          Colors.green.shade100,
          Colors.green,
          isFree,
          _getSelectedPresetName() == 'Fast',
        ),
        const SizedBox(height: 12),
        _buildPresetButton(
          'Balanced',
          'Recommended',
          '\$\$',
          Colors.blue.shade100,
          Colors.blue,
          isFree,
          _getSelectedPresetName() == 'Balanced',
        ),
        const SizedBox(height: 12),
        _buildPresetButton(
          'Smart',
          'Best quality but slow',
          '\$\$\$',
          Colors.purple.shade100,
          Colors.purple,
          isFree,
          _getSelectedPresetName() == 'Smart',
        ),
      ],
    );
  }

  Widget _buildPresetButton(
    String title,
    String description,
    String pricing,
    Color color,
    Color colorSelected,
    bool isFree,
    bool isSelected,
  ) {
    return InkWell(
      onTap:
          isFree
              ? null
              : () {
                setState(() {
                  // Set the models based on the preset
                  final preset = modelPresets[title]!;
                  _selectedModel = preset.text!.name;
                  _selectedVisionModel = preset.vision!.name;
                  _selectedImageGenModel = preset.imageGen!.name;
                });

                // Apply the preset immediately
                widget.onModelSelected(modelPresets[title]!);
                Navigator.pop(context);
              },
      child: Container(
        width: double.infinity,
        padding: const EdgeInsets.all(16),
        decoration: BoxDecoration(
          color: color,
          borderRadius: BorderRadius.circular(8),
          border:
              isSelected
                  ? Border.all(color: colorSelected, width: 3)
                  : Border.all(color: color.withOpacity(0.5)),
        ),
        child: Row(
          children: [
            Expanded(
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  Text(
                    title,
                    style: TextStyle(
                      fontSize: 18,
                      fontWeight: FontWeight.bold,
                      color: Colors.grey[900],
                    ),
                  ),
                  const SizedBox(height: 4),
                  Text(
                    description,
                    style: TextStyle(fontSize: 14, color: Colors.grey.shade700),
                  ),
                ],
              ),
            ),
            Text(
              pricing,
              style: TextStyle(
                color: Colors.grey[900],
                fontSize: 16,
                fontWeight: FontWeight.bold,
              ),
            ),
          ],
        ),
      ),
    );
  }

  Widget _buildCustomTab(bool isFree) {
    return Column(
      children: [
        // Text Model Dropdown
        _buildDropdown(
          label: 'Text Model',
          value: _selectedModel,
          items: _modelOptions,
          onChanged:
              isFree
                  ? (value) {}
                  : (value) {
                    setState(() {
                      _selectedModel = value!;
                    });
                  },
        ),
        const SizedBox(height: 16),

        // Vision Model Dropdown
        _buildDropdown(
          label: 'Vision Model',
          value: _selectedVisionModel,
          items: VisionModelOptions,
          onChanged:
              isFree
                  ? (value) {}
                  : (value) {
                    setState(() {
                      _selectedVisionModel = value!;
                    });
                  },
        ),
        const SizedBox(height: 16),

        // Image Generation Model Dropdown
        _buildDropdown(
          label: 'Image Generation Model',
          value: _selectedImageGenModel,
          items: _imageGenModelOptions,
          onChanged:
              isFree
                  ? (value) {}
                  : (value) {
                    setState(() {
                      _selectedImageGenModel = value!;
                    });
                  },
        ),
      ],
    );
  }

  Widget _buildFunctionsTab() {
    return ListView.builder(
      itemCount: _functions.length,
      itemBuilder: (context, index) {
        final function = _functions[index];
        return SwitchListTile(
          title: Text(
            function['label'] +
                ((function['tools'] != null && function['tools'].length > 1)
                    ? ' (${function['tools'].length} tools) '
                    : ''),
          ),
          subtitle: Text(function['description']),
          value: function['enabled'],
          onChanged: (value) {
            setState(() {
              _functions[index]['enabled'] = value;
            });
          },
        );
      },
    );
  }

  ModelSelected _getSelectedPreset() {
    // Find which preset matches the current selection
    for (var entry in modelPresets.entries) {
      if (entry.value.text?.name == _selectedModel &&
          entry.value.vision?.name == _selectedVisionModel &&
          entry.value.imageGen?.name == _selectedImageGenModel) {
        return entry.value;
      }
    }

    // Default to Balanced if no match
    return modelPresets['Balanced']!;
  }

  Widget _buildDropdown({
    required String label,
    required String value,
    required List<String> items,
    required Function(String?)? onChanged,
  }) {
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        Text(
          label,
          style: const TextStyle(fontSize: 16, fontWeight: FontWeight.w500),
        ),
        const SizedBox(height: 8),
        Container(
          decoration: BoxDecoration(
            border: Border.all(color: Colors.grey.shade300),
            borderRadius: BorderRadius.circular(8),
          ),
          child: DropdownButtonHideUnderline(
            child: DropdownButton<String>(
              value: value,
              isExpanded: true,
              icon: const Icon(Icons.arrow_drop_down),
              padding: const EdgeInsets.symmetric(horizontal: 12),
              borderRadius: BorderRadius.circular(8),
              items:
                  items.map((String item) {
                    return DropdownMenuItem<String>(
                      value: item,
                      child: Text(item),
                    );
                  }).toList(),
              onChanged: onChanged,
            ),
          ),
        ),
      ],
    );
  }
}
