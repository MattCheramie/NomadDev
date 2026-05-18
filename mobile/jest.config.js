module.exports = {
  preset: 'jest-expo',
  setupFiles: ['<rootDir>/jest.setup.ts'],
  moduleNameMapper: {
    '^@/(.*)$': '<rootDir>/src/$1',
  },
  transformIgnorePatterns: [
    'node_modules/(?!((jest-)?react-native|@react-native(-community)?|expo(nent)?|@expo(nent)?/.*|@expo-google-fonts/.*|react-navigation|@react-navigation/.*|@unimodules/.*|unimodules|sentry-expo|native-base|react-native-svg))',
  ],
  testEnvironment: 'jsdom',
  // Playwright specs live in e2e/ and use a different runner. Jest
  // should not try to load them — its testEnvironment is jsdom and
  // @playwright/test pulls in node-only globals that crash here.
  testPathIgnorePatterns: ['/node_modules/', '/e2e/'],
};
