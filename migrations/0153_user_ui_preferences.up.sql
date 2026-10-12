-- Display preferences (10/10/2026): colour theme and interface language,
-- kept on the account so they follow the person to every device. Display
-- only — exported documents and e-mails stay Thai whatever ui_locale says.
--   ui_theme:  'system' follows the device's light/dark setting
--   ui_locale: 'th' or 'en'
ALTER TABLE users
  ADD COLUMN ui_theme TEXT NOT NULL DEFAULT 'system'
    CHECK (ui_theme IN ('light', 'dark', 'system')),
  ADD COLUMN ui_locale TEXT NOT NULL DEFAULT 'th'
    CHECK (ui_locale IN ('th', 'en'));
