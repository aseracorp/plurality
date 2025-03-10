import 'dart:convert';
import 'package:shared_preferences/shared_preferences.dart';
import '../utils/types.dart';

class SharedPreferencesService {
  static const String _selectedModelKey = 'selected_model';
  static const String _darkModeKey = 'dark_mode';
  static const String _useMiniMapKey = 'use_mini_map';

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
}
