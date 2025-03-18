import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import '../../api/balance.dart';
import '../../utils/types.dart';

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
    'llama-v3p1-8b-instruct',
    'llama-v3p2-3b',
    'llama-v3p1-70b-instruct',
    'llama-v3p1-405b-instruct',
    'deepseek-r1',
    'deepseek-v3',
    'qwen2p5-72b-instruct',
    'ChatGPT/gpt-4o-mini',
    'ChatGPT/gpt-3.5-turbo',
    'ChatGPT/gpt-4o',
    'Claude/claude-3-haiku',
    'Claude/claude-3-7-sonnet',
  ];

  final List<String> _visionModelOptions = [
    'llama-v3p2-11b-vision-instruct',
    'llama-v3p2-90b-vision-instruct',
  ];

  final List<String> _imageGenModelOptions = [
    'black-forest-labs/FLUX.1-schnell',
    'black-forest-labs/FLUX.1-dev',
  ];

  final List<Map<String, dynamic>> _functions = [
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
  ];

  String _selectedModel = '';
  String _selectedVisionModel = '';
  String _selectedImageGenModel = '';

  late TabController _tabController;
  int _currentTabIndex = 0;

  // Preset configurations
  static Map<String, ModelSelected> presets = {
    'Fast': ModelSelected(
      text: Model(name: 'llama-v3p1-8b-instruct', params: null, tools: []),
      vision: Model(
        name: 'llama-v3p2-11b-vision-instruct',
        params: null,
        tools: [],
      ),
      imageGen: Model(name: 'black-forest-labs/FLUX.1-schnell', params: null),
    ),
    'Balanced': ModelSelected(
      text: Model(
        name: 'llama-v3p1-70b-instruct',
        params: null,
        tools: ['search_web', 'place_search', 'visit_link'],
      ),
      vision: Model(
        name: 'llama-v3p2-90b-vision-instruct',
        params: null,
        tools: ['search_web', 'place_search', 'visit_link'],
      ),
      imageGen: Model(name: 'black-forest-labs/FLUX.1-schnell', params: null),
    ),
    'Smart': ModelSelected(
      text: Model(
        name: 'deepseek-v3',
        params: null,
        tools: ['search_web', 'place_search', 'visit_link'],
      ),
      vision: Model(
        name: 'llama-v3p2-90b-vision-instruct',
        params: null,
        tools: ['search_web', 'place_search', 'visit_link'],
      ),
      imageGen: Model(name: 'black-forest-labs/FLUX.1-dev', params: null),
    ),
  };

  static getFastPreset() => presets['Fast']!;

  @override
  void initState() {
    super.initState();
    _tabController = TabController(
      length: 3,
      vsync: this,
    ); // Change length to 3
    _tabController.addListener(_handleTabChange);

    _selectedModel = widget.selectedModel.text?.name ?? _modelOptions.first;
    _selectedVisionModel =
        widget.selectedModel.vision?.name ?? _visionModelOptions.first;
    _selectedImageGenModel =
        widget.selectedModel.imageGen?.name ?? _imageGenModelOptions.first;

    if (widget.selectedModel.text?.tools != null) {
      for (var function in _functions) {
        function['enabled'] = widget.selectedModel.text!.tools!.contains(
          function['key'],
        );
      }
    }

    // Check if user is free and set default models if needed
    WidgetsBinding.instance.addPostFrameCallback((_) {
      _checkAndSetDefaultModels();
    });
  }

  String _getSelectedPresetName() {
    // Find which preset matches the current selection
    for (var entry in presets.entries) {
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
      final defaultVisionModel = 'llama-v3p2-11b-vision-instruct';
      final defaultImageGenModel = 'black-forest-labs/FLUX.1-schnell';
      final defaultTextModel = 'llama-v3p1-8b-instruct';

      if (_selectedVisionModel != defaultVisionModel ||
          _selectedImageGenModel != defaultImageGenModel ||
          _selectedModel != defaultTextModel) {
        setState(() {
          _selectedVisionModel = defaultVisionModel;
          _selectedImageGenModel = defaultImageGenModel;
          _selectedModel = defaultTextModel;
        });

        // Call setModel to update the selected models
        widget.onModelSelected(
          ModelSelected(
            text: Model(name: _selectedModel, params: null),
            vision: Model(name: _selectedVisionModel, params: null),
            imageGen: Model(name: _selectedImageGenModel, params: null),
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
    if (!_visionModelOptions.contains(_selectedVisionModel)) {
      setState(() {
        _selectedVisionModel = _visionModelOptions.first;
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
                Tab(text: 'Functions'), // Add the third tab
              ],
              labelColor: Color.fromARGB(255, 204, 52, 65),
              unselectedLabelColor: isDarkMode ? Colors.white : Colors.black,
              indicatorColor: Color.fromARGB(255, 204, 52, 65),
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
                                      .map(
                                        (function) => function['key'] as String,
                                      )
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
                  final preset = presets[title]!;
                  _selectedModel = preset.text!.name;
                  _selectedVisionModel = preset.vision!.name;
                  _selectedImageGenModel = preset.imageGen!.name;
                });

                // Apply the preset immediately
                widget.onModelSelected(presets[title]!);
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
          items: _visionModelOptions,
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
          title: Text(function['label']),
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
    for (var entry in presets.entries) {
      if (entry.value.text?.name == _selectedModel &&
          entry.value.vision?.name == _selectedVisionModel &&
          entry.value.imageGen?.name == _selectedImageGenModel) {
        return entry.value;
      }
    }

    // Default to Balanced if no match
    return presets['Balanced']!;
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
