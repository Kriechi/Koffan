package main

import (
	"context"
	"embed"
	"encoding/json"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"shopping-list/api"
	"shopping-list/db"
	"shopping-list/handlers"
	"shopping-list/i18n"
	"shopping-list/webhook"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/compress"
	"github.com/gofiber/fiber/v2/middleware/filesystem"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/gofiber/template/html/v2"
)

//go:embed templates/*
var embeddedTemplatesFS embed.FS

//go:embed static/*
var embeddedStaticFS embed.FS

func main() {
	// Initialize i18n first (before db, so migrations can use translations)
	if err := i18n.Init(); err != nil {
		log.Fatal("Failed to initialize i18n:", err)
	}

	// Set default language from env var (if specified)
	if lang := os.Getenv("DEFAULT_LANG"); lang != "" {
		i18n.SetDefaultLang(lang)
	}

	// Initialize database
	db.Init()
	defer db.Close()

	if err := webhook.ConfigureFromEnv(db.DB); err != nil {
		log.Printf("Outbound webhooks are disabled: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := webhook.Shutdown(ctx); err != nil {
			log.Printf("Webhook worker shutdown timed out: %v", err)
		}
	}()

	// Say what the accounts setting resolved to: MULTI_USER only counts as exactly "true".
	switch v := os.Getenv("MULTI_USER"); {
	case v == "true" && os.Getenv("DISABLE_AUTH") == "true":
		log.Println("MULTI_USER=true is ignored because DISABLE_AUTH=true")
	case v == "true":
		log.Println("Accounts and workspaces are enabled")
	case v != "" && v != "false":
		log.Printf("MULTI_USER=%q is ignored: use exactly \"true\" to enable accounts", v)
	}

	// With accounts enabled, make sure someone can sign in.
	if handlers.MultiUser() {
		admin := strings.TrimSpace(os.Getenv("ADMIN_USER"))
		if admin == "" {
			admin = "admin"
		}
		if db.CountUsers() == 0 {
			if len(admin) > handlers.MaxUsernameLength || !handlers.ValidUsername(admin) {
				log.Fatalf("ADMIN_USER=%q is not a valid username: use up to %d characters without spaces or commas", admin, handlers.MaxUsernameLength)
			}
			password := os.Getenv("APP_PASSWORD")
			if len(password) < handlers.MinPasswordLength {
				log.Fatalf("MULTI_USER=true needs APP_PASSWORD (at least %d characters) to create the first administrator", handlers.MinPasswordLength)
			}
			if err := db.EnsureBootstrapAdmin(admin, password); err != nil {
				log.Fatalf("Could not create the first administrator: %v", err)
			}
			log.Printf("Created administrator %q with the password from APP_PASSWORD", admin)
			if password == handlers.DefaultPassword {
				log.Printf("WARNING: administrator %q uses the built-in default password; sign in and change it under Your account on the Workspaces page", admin)
			}
		}
	}

	if v := os.Getenv("API_WORKSPACE_ID"); v != "" {
		if !handlers.MultiUser() {
			log.Println("API_WORKSPACE_ID is ignored unless MULTI_USER=true")
		} else if id, err := strconv.ParseInt(v, 10, 64); err != nil || id < 1 {
			log.Fatalf("API_WORKSPACE_ID=%q is not a workspace id", v)
		} else if _, err := db.GetWorkspace(id); err != nil {
			log.Printf("API_WORKSPACE_ID=%d: that workspace does not exist; the REST API answers 503 until it does", id)
		}
	}

	// Clean expired sessions on startup
	db.CleanExpiredSessions()

	// Initialize login rate limiter
	handlers.InitLoginRateLimiter()

	// Initialize template engine
	templatesRootFS, err := fs.Sub(embeddedTemplatesFS, "templates")
	if err != nil {
		log.Fatalf("Embedded templates directory missing: %v", err)
	}

	engine := html.NewFileSystem(http.FS(templatesRootFS), ".html")
	engine.Reload(os.Getenv("APP_ENV") != "production")

	// Add custom template functions
	engine.AddFuncMap(template.FuncMap{
		"dict": func(values ...interface{}) map[string]interface{} {
			if len(values)%2 != 0 {
				return nil
			}
			dict := make(map[string]interface{}, len(values)/2)
			for i := 0; i < len(values); i += 2 {
				key, ok := values[i].(string)
				if !ok {
					continue
				}
				dict[key] = values[i+1]
			}
			return dict
		},
		"add": func(a, b int) int {
			return a + b
		},
		"sub": func(a, b int) int {
			return a - b
		},
		"mul": func(a, b int) int {
			return a * b
		},
		"div": func(a, b int) int {
			if b == 0 {
				return 0
			}
			return a / b
		},
		"gt": func(a, b int) bool {
			return a > b
		},
		"lt": func(a, b int) bool {
			return a < b
		},
		"eq": func(a, b interface{}) bool {
			return a == b
		},
		"ne": func(a, b interface{}) bool {
			return a != b
		},
		// i18n functions
		"T": i18n.T,
		"toJSON": func(v interface{}) template.JS {
			b, err := json.Marshal(v)
			if err != nil {
				return template.JS("{}")
			}
			return template.JS(b)
		},
		"asset": func(path string) string {
			return "/static/" + path + "?v=" + handlers.AssetHash
		},
		// initial is the first letter of a name, for avatar tiles.
		"initial": func(name string) string {
			for _, r := range strings.TrimSpace(name) {
				return strings.ToUpper(string(r))
			}
			return "?"
		},
	})

	// The request read buffer also caps the maximum size of the HTTP
	// request header block. fasthttp's default of 4 KiB is too small when
	// Koffan runs behind an SSO reverse proxy, where the browser sends
	// large cookie headers on every request. Oversized headers are then
	// rejected with HTTP 431 "Request Header Fields Too Large" before the
	// app can handle them. Allow operators to raise the limit.
	readBufferSize := 16384 // 16 KiB default
	if v := os.Getenv("HTTP_READ_BUFFER_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			readBufferSize = n
		} else {
			log.Printf("Invalid HTTP_READ_BUFFER_SIZE %q, using default %d", v, readBufferSize)
		}
	}

	// Initialize Fiber app
	app := fiber.New(fiber.Config{
		Views:          engine,
		ViewsLayout:    "layout",
		ReadBufferSize: readBufferSize,
	})

	// Middleware
	app.Use(logger.New())
	app.Use(recover.New())
	app.Use(compress.New(compress.Config{Level: compress.LevelBestSpeed}))

	// Static files
	staticRootFS, err := fs.Sub(embeddedStaticFS, "static")
	if err != nil {
		log.Fatalf("Embedded static directory missing: %v", err)
	}

	// Compute content hash across embedded static FS. Used as ?v=<hash>
	// cache-buster in templates and injected into sw.js placeholders so
	// any change to static/ automatically invalidates browser + SW caches.
	hash, err := handlers.ComputeAssetHash(staticRootFS)
	if err != nil {
		log.Fatalf("Failed to compute asset hash: %v", err)
	}
	handlers.AssetHash = hash
	log.Printf("Asset hash: %s", hash)

	swBytes, err := handlers.BuildServiceWorker(staticRootFS, hash)
	if err != nil {
		log.Fatalf("Failed to build service worker: %v", err)
	}
	handlers.ServiceWorkerBytes = swBytes

	// SW must be served by a dedicated handler (not the filesystem middleware)
	// so placeholders get replaced and Cache-Control is no-cache instead of
	// the 30-day max-age applied to other static assets.
	app.Get("/static/sw.js", handlers.ServeServiceWorker)

	app.Use("/static", filesystem.New(filesystem.Config{
		Root:   http.FS(staticRootFS),
		Browse: false,
		MaxAge: 86400 * 30, // 30 days - files are embedded and versioned at build time
	}))

	// Browsers request /favicon.ico regardless of the <link rel="icon"> tags.
	// Serve it before the auth middleware so it never 404s or redirects.
	favicon, err := fs.ReadFile(staticRootFS, "favicon.ico")
	if err != nil {
		log.Fatalf("Embedded favicon missing: %v", err)
	}
	app.Get("/favicon.ico", func(c *fiber.Ctx) error {
		c.Set(fiber.HeaderCacheControl, "public, max-age=2592000")
		c.Type("ico")
		return c.Send(favicon)
	})

	// Auth routes (before middleware)
	app.Get("/login", handlers.LoginPage)
	app.Post("/login", handlers.LoginRateLimitMiddleware, handlers.Login)
	app.Post("/logout", handlers.Logout)

	// i18n API (before auth middleware - needed for login page)
	app.Get("/locales", handlers.GetLocales)

	// REST API (before auth middleware - uses token auth)
	api.Register(app)

	// Public endpoints (no auth required)
	app.Get("/api/version", handlers.GetVersion)

	// Auth middleware for all other routes
	app.Use(handlers.AuthMiddleware)

	handlers.RegisterRoutes(app)

	// Get port from env or default to 3000
	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}

	log.Printf("Starting server on port %s", port)
	if err := app.Listen(":" + port); err != nil {
		log.Printf("Server stopped: %v", err)
	}
}
