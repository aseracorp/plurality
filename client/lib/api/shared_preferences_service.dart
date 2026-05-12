import 'dart:convert';
import 'package:shared_preferences/shared_preferences.dart';
import '../utils/types.dart';

class SharedPreferencesService {
  static const String _selectedModelKey = 'selected_model';
  static const String _darkModeKey = 'dark_mode';
  static const String _useMiniMapKey = 'use_mini_map';
  static const String _zoomFactorKey = 'zoom_factor';
  static const String _shortcutOverridesKey = 'model_shortcut_overrides';

  // Singleton pattern
  static final SharedPreferencesService _instance =
      SharedPreferencesService._internal();

  factory SharedPreferencesService() {
    return _instance;
  }

  SharedPreferencesService._internal();

  // Save selected model
  Future<bool> saveSelectedModel(ModelSelected model) async {
    final prefs = await SharedPreferences.getInstance();
    return await prefs.setString(_selectedModelKey, jsonEncode(model.toJson()));
  }

  // Get selected model
  Future<ModelSelected> getSelectedModel() async {
    final prefs = await SharedPreferences.getInstance();
    final jsonString = prefs.getString(_selectedModelKey);

    if (jsonString == null) {
      return ModelSelected(); // Return default
    }

    try {
      final json = jsonDecode(jsonString);
      return ModelSelected.fromJson(json);
    } catch (e) {
      print('Error parsing selected model: $e');
      return ModelSelected(); // Return default on error
    }
  }

  // Save dark mode
  Future<bool> saveDarkMode(int value) async {
    final prefs = await SharedPreferences.getInstance();
    return await prefs.setInt(_darkModeKey, value);
  }

  // Get dark mode
  Future<int> getDarkMode() async {
    final prefs = await SharedPreferences.getInstance();
    return prefs.getInt(_darkModeKey) ?? 0; // Default to light mode
  }

  // Save mini map preference
  Future<bool> saveUseMiniMap(bool value) async {
    final prefs = await SharedPreferences.getInstance();
    return await prefs.setBool(_useMiniMapKey, value);
  }

  // Get mini map preference
  Future<bool> getUseMiniMap() async {
    final prefs = await SharedPreferences.getInstance();
    return prefs.getBool(_useMiniMapKey) ?? true; // Default to true
  }

  // Save zoom factor
  Future<bool> saveZoomFactor(double value) async {
    final prefs = await SharedPreferences.getInstance();
    return await prefs.setDouble(_zoomFactorKey, value);
  }

  // Get zoom factor
  Future<double> getZoomFactor() async {
    final prefs = await SharedPreferences.getInstance();
    return prefs.getDouble(_zoomFactorKey) ?? -1; // -1 means use platform default
  }

  // Save shortcut overrides
  Future<bool> saveShortcutOverrides(
      Map<String, ModelSelected> overrides) async {
    final prefs = await SharedPreferences.getInstance();
    final encoded = overrides.map((k, v) => MapEntry(k, v.toJson()));
    return await prefs.setString(_shortcutOverridesKey, jsonEncode(encoded));
  }

  // Get shortcut overrides
  Future<Map<String, ModelSelected>> getShortcutOverrides() async {
    final prefs = await SharedPreferences.getInstance();
    final jsonString = prefs.getString(_shortcutOverridesKey);
    if (jsonString == null) return {};
    try {
      final raw = jsonDecode(jsonString) as Map<String, dynamic>;
      return raw.map(
        (k, v) => MapEntry(k, ModelSelected.fromJson(v as Map<String, dynamic>)),
      );
    } catch (e) {
      print('Error parsing shortcut overrides: $e');
      return {};
    }
  }
}
