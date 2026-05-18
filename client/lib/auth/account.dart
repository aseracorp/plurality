import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import '../auth/auth-service.dart';
import '../api/api.dart';
import '../api/models_service.dart';
import '../api/preferences_provider.dart';
import '../widgets/model_config/model_config_form.dart';
import '../widgets/model_config/model_config_helpers.dart';
import 'dart:io' show Platform;

class SettingsScreen extends ConsumerStatefulWidget {
  const SettingsScreen({Key? key}) : super(key: key);

  @override
  ConsumerState<SettingsScreen> createState() => _SettingsScreenState();
}

class _SettingsScreenState extends ConsumerState<SettingsScreen> {
  final AuthService _authService = AuthService();
  final ApiService _apiService = ApiService();
  bool _isLoading = false;
  String? _errorMessage;
  int _selectedTab = 0;

  final TextEditingController _memoryController = TextEditingController();
  String _memoryDefault = '';
  bool _memoryLoading = true;
  bool _memorySaving = false;
  String? _memoryError;
  bool _memoryDirty = false;

  String get _userEmail =>
      _authService.currentUser?.username ?? 'Not logged in';

  @override
  void initState() {
    super.initState();
    _loadMemory();
  }

  @override
  void dispose() {
    _memoryController.dispose();
    super.dispose();
  }

  Future<void> _loadMemory() async {
    try {
      final result = await _apiService.getImportantMemory();
      if (!mounted) return;
      setState(() {
        _memoryController.text = result.memory;
        _memoryDefault = result.defaultMemory;
        _memoryLoading = false;
        _memoryError = null;
        _memoryDirty = false;
      });
    } catch (e) {
      if (!mounted) return;
      setState(() {
        _memoryLoading = false;
        _memoryError = e.toString();
      });
    }
  }

  Future<void> _saveMemory() async {
    setState(() {
      _memorySaving = true;
      _memoryError = null;
    });
    try {
      await _apiService.setImportantMemory(_memoryController.text);
      if (!mounted) return;
      setState(() {
        _memorySaving = false;
        _memoryDirty = false;
      });
      ScaffoldMessenger.of(context).showSnackBar(
        const SnackBar(
          content: Text('Memory saved.'),
          backgroundColor: Colors.green,
        ),
      );
    } catch (e) {
      if (!mounted) return;
      setState(() {
        _memorySaving = false;
        _memoryError = e.toString();
      });
    }
  }

  void _resetMemoryToDefault() {
    setState(() {
      _memoryController.text = _memoryDefault;
      _memoryDirty = true;
    });
  }

  Future<void> _resetPassword() async {
    final oldCtl = TextEditingController();
    final newCtl = TextEditingController();
    final confirmed = await showDialog<bool>(
      context: context,
      builder: (ctx) => AlertDialog(
        title: const Text('Change Password'),
        content: Column(
          mainAxisSize: MainAxisSize.min,
          children: [
            TextField(
              controller: oldCtl,
              obscureText: true,
              decoration: const InputDecoration(labelText: 'Current password'),
            ),
            TextField(
              controller: newCtl,
              obscureText: true,
              decoration: const InputDecoration(labelText: 'New password'),
            ),
          ],
        ),
        actions: [
          TextButton(
            onPressed: () => Navigator.of(ctx).pop(false),
            child: const Text('Cancel'),
          ),
          TextButton(
            onPressed: () => Navigator.of(ctx).pop(true),
            child: const Text('Change'),
          ),
        ],
      ),
    );
    if (confirmed != true) return;

    setState(() {
      _isLoading = true;
      _errorMessage = null;
    });
    try {
      await _authService.changePassword(oldCtl.text, newCtl.text);
      if (mounted) {
        ScaffoldMessenger.of(context).showSnackBar(
          const SnackBar(
            content: Text('Password changed.'),
            backgroundColor: Colors.green,
          ),
        );
      }
    } catch (e) {
      if (mounted) {
        setState(() => _errorMessage = 'Failed: ${e.toString()}');
      }
    } finally {
      if (mounted) setState(() => _isLoading = false);
    }
  }

  Future<void> _logout() async {
    setState(() {
      _isLoading = true;
      _errorMessage = null;
    });

    try {
      await _authService.signOut();
      if (mounted) {
        Navigator.of(context).pushReplacementNamed('/login');
      }
    } catch (e) {
      if (mounted) {
        setState(() {
          _errorMessage = 'Failed to logout: ${e.toString()}';
        });
      }
    } finally {
      if (mounted) {
        setState(() {
          _isLoading = false;
        });
      }
    }
  }

  void _deleteAccount() {
    showDialog(
      context: context,
      builder: (BuildContext context) {
        return AlertDialog(
          title: const Text('Delete Account'),
          content: const Text(
            'Are you sure you want to delete your account? This action cannot be undone and will delete all your data.',
          ),
          actions: [
            TextButton(
              onPressed: () => Navigator.of(context).pop(),
              child: const Text('Cancel'),
            ),
            TextButton(
              onPressed: () {
                _apiService.deleteUser();
                _authService.signOut();
                Navigator.of(context).pushNamed('/');
              },
              child: const Text('Delete', style: TextStyle(color: Colors.red)),
            ),
          ],
        );
      },
    );
  }

  void _changeThemeMode(int? mode) {
    if (mode != null) {
      final preferencesNotifier = ref.read(preferencesProvider.notifier);
      preferencesNotifier.setDarkMode(mode);
    }
  }

  @override
  Widget build(BuildContext context) {
    final tabs = const [
      _SettingsTab(label: 'Account', icon: Icons.person),
      _SettingsTab(label: 'Appearance', icon: Icons.palette),
      _SettingsTab(label: 'Model Shortcuts', icon: Icons.bolt),
    ];

    return Scaffold(
      appBar: AppBar(title: const Text('Settings')),
      body: _isLoading
          ? const Center(child: CircularProgressIndicator())
          : Align(
              alignment: Alignment.topCenter,
              child: ConstrainedBox(
                constraints: const BoxConstraints(maxWidth: 720),
                child: Padding(
                  padding: const EdgeInsets.all(16.0),
                  child: Row(
                    crossAxisAlignment: CrossAxisAlignment.start,
                    children: [
                      SizedBox(
                        width: 160,
                        child: Column(
                          children: [
                            for (var i = 0; i < tabs.length; i++)
                              _buildTabEntry(tabs[i], i),
                          ],
                        ),
                      ),
                      const SizedBox(width: 16),
                      Expanded(
                        child: SingleChildScrollView(
                          child: _buildTabContent(),
                        ),
                      ),
                    ],
                  ),
                ),
              ),
            ),
    );
  }

  Widget _buildTabEntry(_SettingsTab tab, int index) {
    final theme = Theme.of(context);
    final isSelected = _selectedTab == index;
    return Padding(
      padding: const EdgeInsets.only(bottom: 8),
      child: InkWell(
        onTap: () => setState(() => _selectedTab = index),
        borderRadius: BorderRadius.circular(8),
        child: Container(
          padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 12),
          decoration: BoxDecoration(
            color: isSelected
                ? theme.colorScheme.primary.withOpacity(0.15)
                : null,
            borderRadius: BorderRadius.circular(8),
            border: Border.all(
              color: isSelected
                  ? theme.colorScheme.primary
                  : Colors.grey.shade300,
              width: isSelected ? 2 : 1,
            ),
          ),
          child: Row(
            children: [
              Icon(
                tab.icon,
                size: 20,
                color: isSelected ? theme.colorScheme.primary : Colors.grey,
              ),
              const SizedBox(width: 8),
              Flexible(
                child: Text(
                  tab.label,
                  style: TextStyle(
                    color: isSelected
                        ? theme.colorScheme.primary
                        : Colors.grey.shade700,
                    fontWeight:
                        isSelected ? FontWeight.bold : FontWeight.normal,
                  ),
                ),
              ),
            ],
          ),
        ),
      ),
    );
  }

  Widget _buildTabContent() {
    switch (_selectedTab) {
      case 0:
        return _buildAccountTab();
      case 1:
        return _buildAppearanceTab();
      case 2:
        return const _ModelShortcutsTab();
      default:
        return const SizedBox.shrink();
    }
  }

  Widget _buildAccountTab() {
    final colorScheme = Theme.of(context).colorScheme;
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        Card(
          elevation: 2,
          child: Padding(
            padding: const EdgeInsets.all(16.0),
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                const Text(
                  'Account Information',
                  style: TextStyle(fontSize: 18, fontWeight: FontWeight.bold),
                ),
                const SizedBox(height: 16),
                Row(
                  children: [
                    const Icon(Icons.person, size: 24),
                    const SizedBox(width: 8),
                    Expanded(
                      child: Text(
                        'Logged in as: $_userEmail',
                        style: const TextStyle(fontSize: 16),
                        overflow: TextOverflow.ellipsis,
                      ),
                    ),
                  ],
                ),
              ],
            ),
          ),
        ),
        const SizedBox(height: 24),
        const Text(
          'Important Memory',
          style: TextStyle(fontSize: 18, fontWeight: FontWeight.bold),
        ),
        const SizedBox(height: 8),
        Text(
          'A compact snippet injected into the assistant\'s system prompt at '
          'the start of every conversation. The assistant can rewrite it via '
          'a tool; you can edit it here.',
          style: TextStyle(color: Colors.grey.shade700),
        ),
        const SizedBox(height: 12),
        _buildMemoryCard(colorScheme),
        const SizedBox(height: 24),
        const Text(
          'Security',
          style: TextStyle(fontSize: 18, fontWeight: FontWeight.bold),
        ),
        const SizedBox(height: 16),
        SettingsButton(
          icon: Icons.lock_reset,
          label: 'Change Password',
          onTap: _resetPassword,
          color: colorScheme.primary,
        ),
        const SizedBox(height: 24),
        const Text(
          'Account',
          style: TextStyle(fontSize: 18, fontWeight: FontWeight.bold),
        ),
        const SizedBox(height: 16),
        SettingsButton(
          icon: Icons.logout,
          label: 'Logout',
          onTap: _logout,
          color: Colors.orange,
        ),
        const Divider(height: 32, thickness: 1),
        SettingsButton(
          icon: Icons.delete_forever,
          label: 'Delete Account',
          onTap: _deleteAccount,
          color: Colors.red,
        ),
        if (_errorMessage != null) ...[
          const SizedBox(height: 16),
          Container(
            padding: const EdgeInsets.all(8),
            decoration: BoxDecoration(
              color: Colors.red.withOpacity(0.1),
              borderRadius: BorderRadius.circular(8),
            ),
            child: Text(
              _errorMessage!,
              style: const TextStyle(color: Colors.red),
            ),
          ),
        ],
      ],
    );
  }

  Widget _buildMemoryCard(ColorScheme colorScheme) {
    if (_memoryLoading) {
      return const Card(
        margin: EdgeInsets.only(bottom: 12),
        child: Padding(
          padding: EdgeInsets.all(24),
          child: Center(child: CircularProgressIndicator()),
        ),
      );
    }
    return Card(
      margin: const EdgeInsets.only(bottom: 12),
      child: Padding(
        padding: const EdgeInsets.all(16),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            TextField(
              controller: _memoryController,
              minLines: 4,
              maxLines: 12,
              onChanged: (_) {
                if (!_memoryDirty) setState(() => _memoryDirty = true);
              },
              decoration: const InputDecoration(
                border: OutlineInputBorder(),
                hintText: 'What should the assistant always remember?',
              ),
            ),
            if (_memoryError != null) ...[
              const SizedBox(height: 8),
              Text(_memoryError!, style: const TextStyle(color: Colors.red)),
            ],
            const SizedBox(height: 12),
            Row(
              mainAxisAlignment: MainAxisAlignment.end,
              children: [
                TextButton.icon(
                  onPressed: _memorySaving ? null : _resetMemoryToDefault,
                  icon: const Icon(Icons.restore, size: 18),
                  label: const Text('Reset to default'),
                ),
                const SizedBox(width: 8),
                ElevatedButton.icon(
                  onPressed: (_memorySaving || !_memoryDirty) ? null : _saveMemory,
                  icon: _memorySaving
                      ? const SizedBox(
                          width: 16,
                          height: 16,
                          child: CircularProgressIndicator(strokeWidth: 2),
                        )
                      : const Icon(Icons.save, size: 18),
                  label: const Text('Save'),
                  style: ElevatedButton.styleFrom(
                    backgroundColor: colorScheme.primary,
                    foregroundColor: Colors.white,
                  ),
                ),
              ],
            ),
          ],
        ),
      ),
    );
  }

  Widget _buildAppearanceTab() {
    final colorScheme = Theme.of(context).colorScheme;
    final preferences = ref.watch(preferencesProvider);
    final currentThemeMode = preferences.darkMode;
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        const Text(
          'Appearance',
          style: TextStyle(fontSize: 18, fontWeight: FontWeight.bold),
        ),
        const SizedBox(height: 16),
        Card(
          margin: const EdgeInsets.only(bottom: 12),
          child: Padding(
            padding: const EdgeInsets.symmetric(horizontal: 16, vertical: 12),
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                Row(
                  children: [
                    Icon(Icons.brightness_4, color: colorScheme.primary),
                    const SizedBox(width: 16),
                    Text('Theme Mode', style: TextStyle(fontSize: 16)),
                  ],
                ),
                const SizedBox(height: 12),
                Row(
                  mainAxisAlignment: MainAxisAlignment.spaceEvenly,
                  children: [
                    _buildThemeOption(
                      0,
                      'System',
                      Icons.settings_system_daydream,
                      currentThemeMode,
                    ),
                    _buildThemeOption(
                      1,
                      'Light',
                      Icons.light_mode,
                      currentThemeMode,
                    ),
                    _buildThemeOption(
                      2,
                      'Dark',
                      Icons.dark_mode,
                      currentThemeMode,
                    ),
                  ],
                ),
              ],
            ),
          ),
        ),
        const SizedBox(height: 12),
        _buildZoomFactorCard(colorScheme),
      ],
    );
  }

  Widget _buildThemeOption(
    int mode,
    String label,
    IconData icon,
    int currentMode,
  ) {
    final isSelected = mode == currentMode;
    final theme = Theme.of(context);

    return InkWell(
      onTap: () => _changeThemeMode(mode),
      borderRadius: BorderRadius.circular(8),
      child: Container(
        padding: EdgeInsets.symmetric(horizontal: 16, vertical: 12),
        decoration: BoxDecoration(
          color: isSelected ? theme.colorScheme.primary.withOpacity(0.2) : null,
          borderRadius: BorderRadius.circular(8),
          border: Border.all(
            color:
                isSelected ? theme.colorScheme.primary : Colors.grey.shade300,
            width: 2,
          ),
        ),
        child: Column(
          children: [
            Icon(
              icon,
              color: isSelected ? theme.colorScheme.primary : Colors.grey,
              size: 24,
            ),
            const SizedBox(height: 8),
            Text(
              label,
              style: TextStyle(
                color: isSelected ? theme.colorScheme.primary : Colors.grey,
                fontWeight: isSelected ? FontWeight.bold : FontWeight.normal,
              ),
            ),
          ],
        ),
      ),
    );
  }

  double _defaultZoomFactor() {
    final isDesktop =
        !kIsWeb && (Platform.isWindows || Platform.isLinux || Platform.isMacOS);
    return isDesktop ? 1.1 : 1.0;
  }

  Widget _buildZoomFactorCard(ColorScheme colorScheme) {
    final preferences = ref.watch(preferencesProvider);
    final zoomFactor = preferences.zoomFactor < 0
        ? _defaultZoomFactor()
        : preferences.zoomFactor;
    final zoomPercent = (zoomFactor * 100).round();

    return Card(
      margin: const EdgeInsets.only(bottom: 12),
      child: Padding(
        padding: const EdgeInsets.symmetric(horizontal: 16, vertical: 12),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Row(
              children: [
                Icon(Icons.zoom_in, color: colorScheme.primary),
                const SizedBox(width: 16),
                Text('Text Size', style: TextStyle(fontSize: 16)),
                const Spacer(),
                Text(
                  '$zoomPercent%',
                  style: TextStyle(
                    fontSize: 16,
                    fontWeight: FontWeight.bold,
                    color: colorScheme.primary,
                  ),
                ),
              ],
            ),
            Slider(
              value: zoomFactor,
              min: 0.8,
              max: 2.0,
              divisions: 24,
              label: '$zoomPercent%',
              onChanged: (value) {
                final preferencesNotifier =
                    ref.read(preferencesProvider.notifier);
                preferencesNotifier.setZoomFactor(value);
              },
            ),
            Row(
              mainAxisAlignment: MainAxisAlignment.spaceBetween,
              children: [
                Text('80%', style: TextStyle(fontSize: 12, color: Colors.grey)),
                TextButton(
                  onPressed: () {
                    final preferencesNotifier =
                        ref.read(preferencesProvider.notifier);
                    preferencesNotifier.setZoomFactor(_defaultZoomFactor());
                  },
                  child: Text('Reset to default'),
                ),
                Text(
                  '200%',
                  style: TextStyle(fontSize: 12, color: Colors.grey),
                ),
              ],
            ),
          ],
        ),
      ),
    );
  }
}

class _SettingsTab {
  final String label;
  final IconData icon;

  const _SettingsTab({required this.label, required this.icon});
}

class _ModelShortcutsTab extends ConsumerStatefulWidget {
  const _ModelShortcutsTab();

  @override
  ConsumerState<_ModelShortcutsTab> createState() => _ModelShortcutsTabState();
}

class _ModelShortcutsTabState extends ConsumerState<_ModelShortcutsTab> {
  late Future<ModelsData> _modelsFuture;

  @override
  void initState() {
    super.initState();
    _modelsFuture = ModelsService().get();
  }

  Future<void> _editPreset(PresetConfig preset, ModelsData data) async {
    final overrides = ref.read(preferencesProvider).shortcutOverrides;
    final override = overrides[preset.name];
    final effective = override == null
        ? preset.models
        : mergePresetOnto(override, preset.models);

    final formKey = GlobalKey<ModelConfigFormState>();

    final saved = await showDialog<bool>(
      context: context,
      builder: (ctx) {
        return Dialog(
          shape:
              RoundedRectangleBorder(borderRadius: BorderRadius.circular(16.0)),
          child: ConstrainedBox(
            constraints: const BoxConstraints(maxWidth: 500),
            child: Padding(
              padding: const EdgeInsets.all(20),
              child: Column(
                mainAxisSize: MainAxisSize.min,
                children: [
                  Text(
                    'Edit "${preset.name}" Shortcut',
                    style: const TextStyle(
                        fontSize: 20, fontWeight: FontWeight.w600),
                  ),
                  const SizedBox(height: 16),
                  ModelConfigForm(
                    key: formKey,
                    data: data,
                    initial: effective,
                  ),
                  const SizedBox(height: 24),
                  Row(
                    mainAxisAlignment: MainAxisAlignment.end,
                    children: [
                      TextButton(
                        onPressed: () => Navigator.pop(ctx, false),
                        child: const Text('Cancel'),
                      ),
                      const SizedBox(width: 8),
                      ElevatedButton(
                        onPressed: () => Navigator.pop(ctx, true),
                        style: ElevatedButton.styleFrom(
                          backgroundColor: Theme.of(ctx).primaryColor,
                          foregroundColor: Colors.white,
                        ),
                        child: const Text('Save'),
                      ),
                    ],
                  ),
                ],
              ),
            ),
          ),
        );
      },
    );

    if (saved == true) {
      final result = formKey.currentState?.commit();
      if (result != null) {
        final sparse = diffPresetAgainst(result, preset.models);
        await ref
            .read(preferencesProvider.notifier)
            .setShortcutOverride(preset.name, sparse);
      }
    }
  }

  Future<void> _resetPreset(PresetConfig preset) async {
    await ref
        .read(preferencesProvider.notifier)
        .clearShortcutOverride(preset.name);
  }

  @override
  Widget build(BuildContext context) {
    return FutureBuilder<ModelsData>(
      future: _modelsFuture,
      builder: (context, snapshot) {
        if (snapshot.connectionState != ConnectionState.done) {
          return const SizedBox(
            height: 200,
            child: Center(child: CircularProgressIndicator()),
          );
        }
        if (snapshot.hasError) {
          return SizedBox(
            height: 200,
            child: Center(
              child: Column(
                mainAxisAlignment: MainAxisAlignment.center,
                children: [
                  const Icon(Icons.error_outline,
                      color: Colors.red, size: 36),
                  const SizedBox(height: 8),
                  Text(snapshot.error.toString()),
                  const SizedBox(height: 8),
                  ElevatedButton(
                    onPressed: () => setState(() {
                      _modelsFuture = ModelsService().get();
                    }),
                    child: const Text('Retry'),
                  ),
                ],
              ),
            ),
          );
        }
        final data = snapshot.data!;
        final overrides = ref.watch(preferencesProvider).shortcutOverrides;

        return Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            const Text(
              'Model Shortcuts',
              style: TextStyle(fontSize: 18, fontWeight: FontWeight.bold),
            ),
            const SizedBox(height: 8),
            Text(
              'Customize the models and tools used for each shortcut. '
              'Overrides apply when you tap a shortcut in the model picker.',
              style: TextStyle(color: Colors.grey.shade700),
            ),
            const SizedBox(height: 16),
            for (final preset in data.presets) ...[
              _ShortcutCard(
                preset: preset,
                isOverridden: overrides.containsKey(preset.name),
                onEdit: () => _editPreset(preset, data),
                onReset: () => _resetPreset(preset),
              ),
              const SizedBox(height: 12),
            ],
          ],
        );
      },
    );
  }
}

class _ShortcutCard extends StatelessWidget {
  final PresetConfig preset;
  final bool isOverridden;
  final VoidCallback onEdit;
  final VoidCallback onReset;

  const _ShortcutCard({
    required this.preset,
    required this.isOverridden,
    required this.onEdit,
    required this.onReset,
  });

  @override
  Widget build(BuildContext context) {
    final base = colorFromName(preset.color);
    return Container(
      padding: const EdgeInsets.all(16),
      decoration: BoxDecoration(
        color: base.shade100,
        borderRadius: BorderRadius.circular(8),
        border: Border.all(color: base.shade100.withOpacity(0.5)),
      ),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Row(
            children: [
              Expanded(
                child: Column(
                  crossAxisAlignment: CrossAxisAlignment.start,
                  children: [
                    Row(
                      children: [
                        Text(
                          preset.name,
                          style: TextStyle(
                            fontSize: 18,
                            fontWeight: FontWeight.bold,
                            color: Colors.grey[900],
                          ),
                        ),
                        if (isOverridden) ...[
                          const SizedBox(width: 8),
                          Container(
                            padding: const EdgeInsets.symmetric(
                                horizontal: 8, vertical: 2),
                            decoration: BoxDecoration(
                              color: base,
                              borderRadius: BorderRadius.circular(12),
                            ),
                            child: const Text(
                              'Customized',
                              style: TextStyle(
                                fontSize: 11,
                                color: Colors.white,
                                fontWeight: FontWeight.w600,
                              ),
                            ),
                          ),
                        ],
                      ],
                    ),
                    const SizedBox(height: 4),
                    Text(
                      preset.label,
                      style: TextStyle(
                          fontSize: 14, color: Colors.grey.shade700),
                    ),
                  ],
                ),
              ),
              Text(
                preset.pricing,
                style: TextStyle(
                  color: Colors.grey[900],
                  fontSize: 14,
                  fontWeight: FontWeight.bold,
                ),
              ),
            ],
          ),
          const SizedBox(height: 12),
          Row(
            mainAxisAlignment: MainAxisAlignment.end,
            children: [
              if (isOverridden)
                TextButton.icon(
                  onPressed: onReset,
                  icon: const Icon(Icons.restore, size: 18),
                  label: const Text('Reset'),
                ),
              const SizedBox(width: 8),
              ElevatedButton.icon(
                onPressed: onEdit,
                icon: const Icon(Icons.edit, size: 18),
                label: const Text('Edit'),
                style: ElevatedButton.styleFrom(
                  backgroundColor: base,
                  foregroundColor: Colors.white,
                ),
              ),
            ],
          ),
        ],
      ),
    );
  }
}

class SettingsButton extends StatelessWidget {
  final IconData icon;
  final String label;
  final VoidCallback onTap;
  final Color color;

  const SettingsButton({
    Key? key,
    required this.icon,
    required this.label,
    required this.onTap,
    required this.color,
  }) : super(key: key);

  @override
  Widget build(BuildContext context) {
    return Card(
      margin: const EdgeInsets.only(bottom: 12),
      child: InkWell(
        onTap: onTap,
        borderRadius: BorderRadius.circular(4),
        child: Padding(
          padding: const EdgeInsets.symmetric(horizontal: 16, vertical: 12),
          child: Row(
            children: [
              Icon(icon, color: color),
              const SizedBox(width: 16),
              Text(
                label,
                style: TextStyle(
                  fontSize: 16,
                  color: color == Colors.red ? Colors.red : null,
                ),
              ),
              const Spacer(),
              Icon(Icons.arrow_forward_ios, size: 16, color: Colors.grey[600]),
            ],
          ),
        ),
      ),
    );
  }
}
