# Translation System (i18n)

## Adding a New Language

### 1. Create a JSON File

Copy `en.json` as a template and create a new file, e.g., `da.json` for Danish:

```bash
cp i18n/en.json i18n/da.json
```

### 2. Edit Metadata

At the beginning of the file, modify the `meta` section:

```json
{
  "meta": {
    "code": "da",
    "name": "Dansk",
    "flag": "DK"
  }
}
```

Use the two-letter language code, the language's native name and its flag code. Update this section in the copied file while keeping all translation sections below it.

### 3. Translate All Keys

Translate values (NOT keys!) in each section:

- `common` - buttons: Add, Cancel, Save, Delete, Edit, Close
- `nav` - navigation: Settings, Logout
- `list` - list: title, empty list, shopping list, completed, Bought
- `items` - products: What to buy?, Note, name, new product, select section
- `sections` - sections: title, new section, section list, manage, select, no sections
- `actions` - actions: move up, move down, move, uncertain, certain
- `settings` - settings: title, language
- `login` - login: title, subtitle, password, placeholder, button, error
- `confirm` - confirmations: delete item, delete sections (with `{{name}}`, `{{count}}` parameters)
- `offline` - offline capabilities, persistence, synchronization errors, retry and discard controls
- Translate the remaining sections in `en.json` too, including lists, history, templates, imports and exports.

### 4. Rebuild the Application

JSON files are embedded in the binary, so after adding/changing translations:

```bash
go build -o shopping-list-go
./shopping-list-go
```

### 5. Done!

The new language will automatically appear in the Settings language selector.

---

## Updating Existing Text

When adding or changing user-facing copy, update every supported language in the same change. This includes the offline modal and synchronization error and confirmation messages. English is the reference catalog; the English fallback handles missing keys at runtime but is not a substitute for a translation.

Keep key names and `{{parameter}}` placeholders unchanged. Run `go test ./i18n/` to check that every catalog has the same keys and placeholders as `en.json` and no empty values, then check that the rebuilt app serves the updated text. Rebuild and restart after translation changes because the catalogs are embedded in the binary.

## Translation File Structure

The following is a shortened example. Copy the complete `en.json` file when adding a language.

```json
{
  "meta": {
    "code": "xx",
    "name": "Language name",
    "flag": "XX"
  },
  "common": {
    "add": "...",
    "cancel": "...",
    "save": "...",
    "delete": "...",
    "edit": "...",
    "close": "..."
  },
  "nav": {
    "settings": "...",
    "logout": "..."
  },
  "list": {
    "title": "...",
    "empty_list": "...",
    "shopping_list": "...",
    "completed": "...",
    "bought": "..."
  },
  "items": {
    "what_to_buy": "...",
    "note": "...",
    "note_optional": "...",
    "name": "...",
    "new_product": "...",
    "select_section": "...",
    "section": "..."
  },
  "sections": {
    "title": "...",
    "new_section": "...",
    "section_list": "...",
    "manage": "...",
    "select": "...",
    "no_sections": "...",
    "add_first_section": "..."
  },
  "actions": {
    "move_up": "...",
    "move_down": "...",
    "move": "...",
    "uncertain": "...",
    "certain": "...",
    "remove_mark": "...",
    "mark_uncertain": "..."
  },
  "settings": {
    "title": "...",
    "language": "...",
    "coming_soon": "..."
  },
  "login": {
    "title": "...",
    "subtitle": "...",
    "password": "...",
    "password_placeholder": "...",
    "submit": "...",
    "error_invalid": "..."
  },
  "confirm": {
    "delete_item": "... \"{{name}}\"?",
    "delete_sections": "... {{count}} ...?",
    "delete_section": "... '{{name}}'?"
  }
}
```

## Supported Languages

The app currently includes these 18 catalogs. Use the `meta.code` value for `DEFAULT_LANG` and language selection; Ukrainian uses code `uk` even though its file is named `ua.json`.

| Code | Language | File |
|------|----------|------|
| `cs` | Čeština | `cs.json` |
| `de` | Deutsch | `de.json` |
| `el` | Ελληνικά | `el.json` |
| `en` | English | `en.json` |
| `es` | Español | `es.json` |
| `fa` | فارسی | `fa.json` |
| `fr` | Français | `fr.json` |
| `it` | Italiano | `it.json` |
| `lt` | Lietuvių | `lt.json` |
| `nl` | Nederlands/Vlaams | `nl.json` |
| `no` | Norsk | `no.json` |
| `pl` | Polski | `pl.json` |
| `pt` | Português | `pt.json` |
| `ru` | Русский | `ru.json` |
| `sk` | Slovenčina | `sk.json` |
| `sv` | Svenska | `sv.json` |
| `uk` | Українська | `ua.json` |
| `zh` | 中文 | `zh.json` |

## Parameters in Translations

Some texts contain parameters in `{{param}}` format:

- `{{name}}` - element name (item, section)
- `{{count}}` - number of elements

Example:
```json
{
  "delete_item": "Delete \"{{name}}\"?"
}
```

In JS code called as:
```javascript
t('confirm.delete_item', { name: 'Milk' })
// Result: Delete "Milk"?
```
