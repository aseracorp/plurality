import 'package:flutter_riverpod/flutter_riverpod.dart';
import './shared_preferences_service.dart';
import '../utils/types.dart';

// Provider for the SharedPreferences service
final sharedPrefsServiceProvider = Provider<SharedPreferencesService>((ref) {
  return SharedPreferencesService();
});

// Unified preferences provider
final preferencesProvider =
    StateNotifierProvider<PreferencesNotifier, AppPreferences>((ref) {
      final prefsService = ref.watch(sharedPrefsServiceProvider);
      return PreferencesNotifier(prefsService);
    });

class PreferencesNotifier extends StateNotifier<AppPreferences> {
  final SharedPreferencesService _prefsService;

  PreferencesNotifier(this._prefsService) : super(AppPreferences()) {
    loadAllPreferences();
  }

  Future<void> loadAllPreferences() async {
    final selectedModel = await _prefsService.getSelectedModel();
    final darkMode = await _prefsService.getDarkMode();
    final useMiniMap = await _prefsService.getUseMiniMap();
    final zoomFactor = await _prefsService.getZoomFactor();
    final shortcutOverrides = await _prefsService.getShortcutOverrides();

    state = AppPreferences(
      selectedModel: selectedModel,
      darkMode: darkMode,
      useMiniMap: useMiniMap,
      zoomFactor: zoomFactor,
      shortcutOverrides: shortcutOverrides,
    );
  }

  // Update selected model
  Future<void> setSelectedModel(ModelSelected model) async {
    await _prefsService.saveSelectedModel(model);
    state = state.copyWith(selectedModel: model);
  }

  // Update dark mode
  Future<void> setDarkMode(int mode) async {
    await _prefsService.saveDarkMode(mode);
    state = state.copyWith(darkMode: mode);
  }

  // Update mini map preference
  Future<void> setUseMiniMap(bool use) async {
    await _prefsService.saveUseMiniMap(use);
    state = state.copyWith(useMiniMap: use);
  }

  // Update zoom factor
  Future<void> setZoomFactor(double factor) async {
    await _prefsService.saveZoomFactor(factor);
    state = state.copyWith(zoomFactor: factor);
  }

  Future<void> setShortcutOverride(String name, ModelSelected model) async {
    final updated = Map<String, ModelSelected>.from(state.shortcutOverrides);
    updated[name] = model;
    await _prefsService.saveShortcutOverrides(updated);
    state = state.copyWith(shortcutOverrides: updated);
  }

  Future<void> clearShortcutOverride(String name) async {
    if (!state.shortcutOverrides.containsKey(name)) return;
    final updated = Map<String, ModelSelected>.from(state.shortcutOverrides);
    updated.remove(name);
    await _prefsService.saveShortcutOverrides(updated);
    state = state.copyWith(shortcutOverrides: updated);
  }
}
