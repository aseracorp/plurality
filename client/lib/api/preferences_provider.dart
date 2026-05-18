import 'package:flutter_riverpod/flutter_riverpod.dart';
import './models_service.dart';
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
    var selectedModel = await _prefsService.getSelectedModel();
    final darkMode = await _prefsService.getDarkMode();
    final useMiniMap = await _prefsService.getUseMiniMap();
    final zoomFactor = await _prefsService.getZoomFactor();

    // First-boot fallback: nothing persisted yet (no text model means the
    // stored value is the empty `ModelSelected()` default). Seed from the
    // server's Fast preset so the user can send a message without first
    // opening the model picker.
    if (selectedModel.text == null) {
      try {
        final data = await ModelsService().get();
        final fast = data.fastPreset;
        if (fast != null) {
          selectedModel = fast.models;
          await _prefsService.saveSelectedModel(selectedModel);
        }
      } catch (_) {
        // Auth not ready or models endpoint unavailable — leave the empty
        // default; the model picker will populate it when the user opens it.
      }
    }

    state = AppPreferences(
      selectedModel: selectedModel,
      darkMode: darkMode,
      useMiniMap: useMiniMap,
      zoomFactor: zoomFactor,
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
}
