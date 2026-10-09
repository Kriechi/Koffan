package handlers

import (
	"shopping-list/i18n"

	"github.com/gofiber/fiber/v2"
)

// getLang extracts the user's language from the lang cookie
func getLang(c *fiber.Ctx) string {
	if lang := c.Cookies("lang"); lang != "" {
		return lang
	}
	return i18n.GetDefaultLang()
}

// withPageI18n adds the translation data every full page embeds.
func withPageI18n(c *fiber.Ctx, data fiber.Map) fiber.Map {
	lang := i18n.ResolveLang(c.Cookies("lang"))
	data["Translations"] = i18n.PageTranslationsJSON(lang)
	data["PageLang"] = lang
	data["Locales"] = i18n.AvailableLocales()
	data["DefaultLang"] = i18n.GetDefaultLang()
	addWorkspaceContext(c, data)
	return data
}

// sendError sends a translated error message
func sendError(c *fiber.Ctx, status int, key string) error {
	return c.Status(status).SendString(i18n.Get(getLang(c), key))
}
