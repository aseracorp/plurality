import 'package:flutter/material.dart';
import '../../utils/types.dart';
import 'package:flutter/material.dart';
import 'dart:convert';

class MiniAppForm extends StatelessWidget {
  final MiniApp app;
  final Function(String) onChanged;

  const MiniAppForm({Key? key, required this.app, required this.onChanged})
    : super(key: key);

  @override
  Widget build(BuildContext context) {
    /*var form = [
      {
        'type': 'text',
        'label': 'Name',
        'name': 'name',
        'placeholder': 'Enter your name',
        'required': true,
      },
      {
        'type': 'email',
        'label': 'Email',
        'name': 'email',
        'placeholder': 'Enter your email',
        'required': true,
      },
      {
        'type': 'select',
        'label': 'Country',
        'name': 'country',
        'required': true,
        'options': [
          {'value': '', 'label': ''},
          {'value': 'US', 'label': 'United States'},
          {'value': 'CA', 'label': 'Canada'},
        ],
      },
      {
        'type': 'checkbox',
        'label': 'Subscribe to newsletter',
        'name': 'subscribe',
        'value': "subscribed",
      },
      {
        'type': 'radio',
        'label': 'Gender',
        'name': 'gender',
        'options': ["male", "female", "other"],
      },
      {
        'type': 'textarea',
        'label': 'Message',
        'name': 'message',
        'placeholder': 'Enter your message',
        'required': true,
      },
      {
        'type': 'repeater',
        'label': 'Skills',
        'name': 'skills',
        'placeholder': 'Enter your skills',
        'required': true,
        'content': [
          {
            'type': 'text',
            'label': 'Skill Name',
            'name': 'skill-name',
            'placeholder': 'name',
            'required': true,
          },
          {
            'type': 'text',
            'label': 'Skill Description',
            'name': 'skill-description',
            'placeholder': 'description',
            'required': true,
          },
        ],
      },
    ];*/

    if (app.form.isEmpty) {
      return const SizedBox();
    }

    var _form = jsonDecode(app.form) as List<dynamic>;

    // Convert the dynamic list to a list of maps
    List<Map<String, dynamic>> form =
        _form.map((e) => e as Map<String, dynamic>).toList();

    return SizedBox(
      width: 800,
      child: Card(
        margin: const EdgeInsets.symmetric(horizontal: 24, vertical: 8),
        child: Padding(
          padding: const EdgeInsets.all(16.0),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              const SizedBox(height: 16),
              DynamicForm(
                formFields: form,
                onSubmit: (formData) {
                  // For each input
                  var result = "<hidden>\n";
                  for (var field in form) {
                    final String name = field['name'] as String;
                    final String prompt = field['prompt'] as String;
                    final dynamic value = formData[name];

                    if (value != null) {
                      result += "$prompt $value\n";
                    }
                  }

                  result += "</hidden>";

                  print(result);

                  onChanged(result);
                },
              ),
            ],
          ),
        ),
      ),
    );
  }
}

class DynamicForm extends StatefulWidget {
  final List<Map<String, dynamic>> formFields;
  final Function(Map<String, dynamic>) onSubmit;
  final String submitButtonText;

  const DynamicForm({
    Key? key,
    required this.formFields,
    required this.onSubmit,
    this.submitButtonText = 'Submit',
  }) : super(key: key);

  @override
  State<DynamicForm> createState() => _DynamicFormState();
}

class _DynamicFormState extends State<DynamicForm> {
  final _formKey = GlobalKey<FormState>();
  Map<String, dynamic> _formData = {};
  Map<String, List<Map<String, dynamic>>> _repeaterData = {};

  @override
  void initState() {
    super.initState();
    _initializeFormData();
  }

  void _initializeFormData() {
    for (var field in widget.formFields) {
      final name = field['name'] as String;

      if (field['type'] == 'repeater') {
        _repeaterData[name] = [];
      } else if (field['type'] == 'checkbox') {
        _formData[name] = false;
      } else if (field['type'] == 'radio') {
        _formData[name] = null;
      } else {
        _formData[name] = '';
      }
    }
  }

  @override
  Widget build(BuildContext context) {
    return Form(
      key: _formKey,
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          ..._buildFormFields(),
          /*const SizedBox(height: 20),
          ElevatedButton(
            onPressed: _submitForm,
            child: Text(widget.submitButtonText),
          ),*/
        ],
      ),
    );
  }

  List<Widget> _buildFormFields() {
    List<Widget> formFields = [];

    for (var field in widget.formFields) {
      final Widget fieldWidget = _buildField(field);

      formFields.add(
        Padding(
          padding: const EdgeInsets.only(bottom: 16.0),
          child: fieldWidget,
        ),
      );
    }

    return formFields;
  }

  Widget _buildField(Map<String, dynamic> field) {
    final String type = field['type'] as String;
    final String name = field['name'] as String;
    final String label = field['label'] as String;
    final bool isRequired = field['required'] == true;

    switch (type) {
      case 'text':
      case 'email':
        return _buildTextField(field);
      case 'textarea':
        return _buildTextArea(field);
      case 'select':
        return _buildDropdown(field);
      case 'checkbox':
        return _buildCheckbox(field);
      case 'radio':
        return _buildRadioGroup(field);
      case 'repeater':
        return _buildRepeater(field);
      default:
        return Text('Unsupported field type: $type');
    }
  }

  Widget _buildTextField(Map<String, dynamic> field) {
    final String name = field['name'] as String;
    final String label = field['label'] as String;
    final String placeholder = field['placeholder'] as String? ?? '';
    final bool isRequired = field['required'] == true;
    final bool isEmail = field['type'] == 'email';

    return TextFormField(
      decoration: InputDecoration(
        labelText: label,
        hintText: placeholder,
        border: const OutlineInputBorder(),
      ),
      keyboardType: isEmail ? TextInputType.emailAddress : TextInputType.text,
      validator: (value) {
        if (isRequired && (value == null || value.isEmpty)) {
          return '$label is required';
        }
        if (isEmail && value != null && value.isNotEmpty) {
          // Simple email validation
          final emailRegex = RegExp(r'^[\w-\.]+@([\w-]+\.)+[\w-]{2,4}$');
          if (!emailRegex.hasMatch(value)) {
            return 'Please enter a valid email address';
          }
        }
        return null;
      },
      onChanged: (value) {
        setState(() {
          _formData[name] = value;
          _submitForm();
        });
      },
    );
  }

  Widget _buildTextArea(Map<String, dynamic> field) {
    final String name = field['name'] as String;
    final String label = field['label'] as String;
    final String placeholder = field['placeholder'] as String? ?? '';
    final bool isRequired = field['required'] == true;

    return TextFormField(
      decoration: InputDecoration(
        labelText: label,
        hintText: placeholder,
        border: const OutlineInputBorder(),
        alignLabelWithHint: true,
      ),
      maxLines: 5,
      validator: (value) {
        if (isRequired && (value == null || value.isEmpty)) {
          return '$label is required';
        }
        return null;
      },
      onChanged: (value) {
        setState(() {
          _formData[name] = value;
          _submitForm();
        });
      },
    );
  }

  Widget _buildDropdown(Map<String, dynamic> field) {
    final String name = field['name'] as String;
    final String label = field['label'] as String;
    final bool isRequired = field['required'] == true;
    final List<Map<String, dynamic>> options = List<Map<String, dynamic>>.from(
      field['options'],
    );

    return DropdownButtonFormField<String>(
      decoration: InputDecoration(
        labelText: label,
        border: const OutlineInputBorder(),
      ),
      value: _formData[name] != '' ? _formData[name] : null,
      items:
          options.map((option) {
            return DropdownMenuItem<String>(
              value: option['value'] as String,
              child: Text(option['label'] as String),
            );
          }).toList(),
      validator: (value) {
        if (isRequired && (value == null || value.isEmpty)) {
          return '$label is required';
        }
        return null;
      },
      onChanged: (value) {
        setState(() {
          _formData[name] = value;
          _submitForm();
        });
      },
    );
  }

  Widget _buildCheckbox(Map<String, dynamic> field) {
    final String name = field['name'] as String;
    final String label = field['label'] as String;
    final String value = field['value'] as String? ?? 'true';

    return CheckboxListTile(
      title: Text(label),
      value: _formData[name] == true || _formData[name] == value,
      controlAffinity: ListTileControlAffinity.leading,
      onChanged: (bool? newValue) {
        setState(() {
          _formData[name] = newValue == true ? value : "";
          _submitForm();
        });
      },
    );
  }

  Widget _buildRadioGroup(Map<String, dynamic> field) {
    final String name = field['name'] as String;
    final String label = field['label'] as String;
    final List<dynamic> options = field['options'] as List<dynamic>;

    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        Text(
          label,
          style: const TextStyle(fontSize: 16, fontWeight: FontWeight.bold),
        ),
        const SizedBox(height: 8),
        ...options.map((option) {
          final String optionValue = option.toString();
          return RadioListTile<String>(
            title: Text(optionValue),
            value: optionValue,
            groupValue: _formData[name],
            onChanged: (value) {
              setState(() {
                _formData[name] = value;
                _submitForm();
              });
            },
          );
        }).toList(),
      ],
    );
  }

  Widget _buildRepeater(Map<String, dynamic> field) {
    final String name = field['name'] as String;
    final String label = field['label'] as String;
    final List<Map<String, dynamic>> content = List<Map<String, dynamic>>.from(
      field['content'],
    );

    if (!_repeaterData.containsKey(name)) {
      _repeaterData[name] = [];
    }

    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        Row(
          mainAxisAlignment: MainAxisAlignment.spaceBetween,
          children: [
            Text(
              label,
              style: const TextStyle(fontSize: 18, fontWeight: FontWeight.bold),
            ),
            ElevatedButton.icon(
              icon: const Icon(Icons.add),
              label: const Text('Add'),
              onPressed: () {
                setState(() {
                  _repeaterData[name]!.add({});
                  _submitForm();
                });
              },
            ),
          ],
        ),
        const SizedBox(height: 8),
        ..._repeaterData[name]!.asMap().entries.map((entry) {
          final int index = entry.key;
          final Map<String, dynamic> item = entry.value;

          return Card(
            margin: const EdgeInsets.only(bottom: 16),
            child: Padding(
              padding: const EdgeInsets.all(16),
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  Row(
                    mainAxisAlignment: MainAxisAlignment.spaceBetween,
                    children: [
                      Text(
                        'Item ${index + 1}',
                        style: const TextStyle(fontWeight: FontWeight.bold),
                      ),
                      IconButton(
                        icon: const Icon(Icons.delete, color: Colors.red),
                        onPressed: () {
                          setState(() {
                            _repeaterData[name]!.removeAt(index);
                          });
                        },
                      ),
                    ],
                  ),
                  const Divider(),
                  ...content.map((subField) {
                    final String subName = subField['name'] as String;
                    final String subLabel = subField['label'] as String;
                    final String subType = subField['type'] as String;

                    // Create a copy of the field with a unique name for the repeater
                    final Map<String, dynamic> fieldCopy = Map.from(subField);

                    // Handle field value changes within the repeater
                    if (subType == 'text' ||
                        subType == 'email' ||
                        subType == 'textarea') {
                      return TextFormField(
                        decoration: InputDecoration(
                          labelText: subLabel,
                          hintText: subField['placeholder'] as String? ?? '',
                          border: const OutlineInputBorder(),
                        ),
                        initialValue: item[subName] as String? ?? '',
                        validator: (value) {
                          if (subField['required'] == true &&
                              (value == null || value.isEmpty)) {
                            return '$subLabel is required';
                          }
                          return null;
                        },
                        onChanged: (value) {
                          setState(() {
                            _repeaterData[name]![index][subName] = value;
                            _submitForm();
                          });
                        },
                      );
                    }

                    // Add support for other field types as needed
                    return const Text('Unsupported field type in repeater');
                  }).toList(),
                ],
              ),
            ),
          );
        }).toList(),
      ],
    );
  }

  void _submitForm() {
    if (_formKey.currentState!.validate()) {
      // Create a copy of the form data
      final Map<String, dynamic> formData = Map.from(_formData);

      // Add repeater data
      _repeaterData.forEach((key, value) {
        formData[key] = value;
      });

      widget.onSubmit(formData);
    }
  }
}
