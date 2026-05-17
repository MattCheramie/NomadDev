// Web linking config — maps URL paths to navigator routes so the SPA's
// React Navigation stack mirrors the browser history. /chat is the
// authenticated landing pad; /onboard receives QR-onboarded users; /settings
// is reachable via a header button from /chat.

import type { LinkingOptions } from '@react-navigation/native';
import type { RootStackParamList } from './routes';

export const linking: LinkingOptions<RootStackParamList> = {
  prefixes: [],
  config: {
    screens: {
      Onboard: 'onboard',
      Chat: 'chat',
      Settings: 'settings',
    },
  },
};
