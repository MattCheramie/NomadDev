// Uniform async key/value store. On web AsyncStorage transparently routes to
// localStorage; on native it uses the bundled native store. Both are
// fire-and-forget for our purposes (token, last_event_id, server URL).

import AsyncStorage from '@react-native-async-storage/async-storage';

export async function get(key: string): Promise<string | null> {
  try {
    return await AsyncStorage.getItem(key);
  } catch (_e) {
    return null;
  }
}

export async function set(key: string, value: string): Promise<void> {
  try {
    await AsyncStorage.setItem(key, value);
  } catch (_e) {
    /* ignore */
  }
}

export async function remove(key: string): Promise<void> {
  try {
    await AsyncStorage.removeItem(key);
  } catch (_e) {
    /* ignore */
  }
}
