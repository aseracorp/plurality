import 'package:flutter/material.dart';

import '../utils/types.dart';

class ModelSelectionModal extends StatefulWidget {
  final Function(ModelSelected) onModelSelected;
  final ModelSelected selectedModel;

  const ModelSelectionModal({
    Key? key,
    required this.onModelSelected,
    required this.selectedModel,
  }) : super(key: key);

  @override
  _ModelSelectionModalState createState() => _ModelSelectionModalState();
}

class _ModelSelectionModalState extends State<ModelSelectionModal> {
  // Available options for each dropdown
  final List<String> _modelOptions = [
    'meta-llama/Llama-3.2-11B-Vision-Instruct-Turbo',
    'meta-llama/Llama-3.2-8B-Instruct-Turbo',
    'meta-llama/Llama-3.2-3B-Instruct-Turbo',
    'meta-llama/Llama-3.3-70B-Instruct-Turbo',
    'meta-llama/Llama-3.2-90B-Vision-Instruct-Turbo',
    'meta-llama/Meta-Llama-3.1-405B-Instruct-Turbo',
    'deepseek-ai/DeepSeek-R1-Distill-Llama-70B-free',
    'deepseek-ai/DeepSeek-V3',
    'Qwen/Qwen2-VL-72B-Instruct',
    'ChatGPT/gpt-4o-mini',
    'ChatGPT/gpt-3.5-turbo',
    'ChatGPT/gpt-4o',
    'Claude/claude-3-haiku',
    'Claude/claude-3-7-sonnet',
  ];

  final List<String> _visionModelOptions = [
    'meta-llama/Llama-3.2-11B-Vision-Instruct-Turbo',
    'meta-llama/Llama-3.2-90B-Vision-Instruct-Turbo',
  ];

  final List<String> _imageGenModelOptions = [
    'black-forest-labs/FLUX.1-schnell',
    'black-forest-labs/FLUX.1-dev',
    // 'stabilityai/stable-diffusion-xl-base-1.0',
  ];

  String _selectedModel = '';
  String _selectedVisionModel = '';
  String _selectedImageGenModel = '';

  // init

  @override
  void initState() {
    super.initState();
    _selectedModel = widget.selectedModel.text?.name ?? _modelOptions.first;
    _selectedVisionModel =
        widget.selectedModel.vision?.name ?? _visionModelOptions.first;
    _selectedImageGenModel =
        widget.selectedModel.imageGen?.name ?? _imageGenModelOptions.first;
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
    return Container(
      constraints: const BoxConstraints(maxWidth: 400),
      padding: const EdgeInsets.all(20),
      decoration: BoxDecoration(
        shape: BoxShape.rectangle,
        color: Colors.white,
        borderRadius: BorderRadius.circular(16),
        boxShadow: const [
          BoxShadow(
            color: Colors.black26,
            blurRadius: 10.0,
            offset: Offset(0.0, 10.0),
          ),
        ],
      ),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: <Widget>[
          const Text(
            'Select AI Models',
            style: TextStyle(fontSize: 22, fontWeight: FontWeight.w600),
          ),
          const SizedBox(height: 24),

          // Text Model Dropdown
          _buildDropdown(
            label: 'Text Model',
            value: _selectedModel,
            items: _modelOptions,
            onChanged: (value) {
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
            onChanged: (value) {
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
            onChanged: (value) {
              setState(() {
                _selectedImageGenModel = value!;
              });
            },
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
              ElevatedButton(
                onPressed: () {
                  // Return the selected models to the parent widget
                  widget.onModelSelected(
                    ModelSelected(
                      text: Model(name: _selectedModel, params: null),
                      vision: Model(name: _selectedVisionModel, params: null),
                      imageGen: Model(
                        name: _selectedImageGenModel,
                        params: null,
                      ),
                    ),
                  );
                  Navigator.pop(context);
                },
                style: ElevatedButton.styleFrom(
                  backgroundColor: Colors.blue,
                  foregroundColor: Colors.white,
                ),
                child: const Text('Confirm'),
              ),
            ],
          ),
        ],
      ),
    );
  }

  Widget _buildDropdown({
    required String label,
    required String value,
    required List<String> items,
    required Function(String?) onChanged,
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
