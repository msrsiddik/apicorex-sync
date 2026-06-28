// Package plugin is the sync service's self-contained ApiCoreX plugin runtime.
// It serves the manifest/health endpoints, registers with Core over HTTP (with
// backoff + re-register on heartbeat failure), and exposes header-based
// tenant-context helpers. Copied from apicorex-identity's internal/plugin so
// the sync service stays standalone (no shared SDK).
package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	ginopenapi "github.com/oaswrap/spec/adapter/ginopenapi"
	"github.com/oaswrap/spec/option"
	"golang.org/x/sync/errgroup"
)

// Tenant context headers Core injects after verifying the JWT.
const (
	HeaderTenantID   = "X-ApiCoreX-Tenant-ID"
	HeaderTenantSlug = "X-ApiCoreX-Tenant-Slug"
	HeaderSchema     = "X-ApiCoreX-Schema"
	HeaderUserID     = "X-ApiCoreX-User-ID"
	HeaderUserType   = "X-ApiCoreX-User-Type"
	HeaderRoles      = "X-ApiCoreX-Roles"
)

// These helpers read the trusted tenant context Core injected as X-ApiCoreX-*
// headers after verifying the JWT. Handlers call them instead of parsing tokens.
func TenantID(c *gin.Context) string   { return c.GetHeader(HeaderTenantID) }
func TenantSlug(c *gin.Context) string { return c.GetHeader(HeaderTenantSlug) }
func SchemaName(c *gin.Context) string { return c.GetHeader(HeaderSchema) }
func UserID(c *gin.Context) string     { return c.GetHeader(HeaderUserID) }
func UserType(c *gin.Context) string   { return c.GetHeader(HeaderUserType) }
func Roles(c *gin.Context) []string {
	v := c.GetHeader(HeaderRoles)
	if v == "" {
		return nil
	}
	return strings.Split(v, ",")
}

// Config holds plugin runtime settings (from env in main).
type Config struct {
	Name          string
	Version       string
	CoreURL       string
	PluginAddr    string // bind addr, e.g. :50051
	PluginBaseURL string // advertised URL Core dials back
	APIKey        string
}

// Migration mirrors the manifest migration shape.
type Migration struct {
	Version string `json:"version"`
	Name    string `json:"name"`
	UpSQL   string `json:"up_sql"`
	DownSQL string `json:"down_sql"`
}

type route struct {
	Method string   `json:"method"`
	Path   string   `json:"path"`
	Public bool     `json:"public"`
	Tags   []string `json:"tags,omitempty"`
}

// Plugin wraps a real *gin.Engine plus a shadow OpenAPI router for Scalar docs.
type Plugin struct {
	cfg         Config
	engine      *gin.Engine
	spec        ginopenapi.Generator
	routes      []route
	public      map[string]bool
	migrations  []Migration
	pluginID    string
	pluginToken string
}

// New creates a Plugin with its real Gin engine and shadow OpenAPI router.
func New(cfg Config) *Plugin {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())

	spec := ginopenapi.NewRouter(gin.New(),
		option.WithTitle(cfg.Name),
		option.WithVersion(cfg.Version),
		option.WithDisableDocs(),
	)
	return &Plugin{cfg: cfg, engine: engine, spec: spec, public: map[string]bool{}}
}

// Public marks a path as not requiring JWT auth.
func (p *Plugin) Public(path string) { p.public[path] = true }

// Migration declares a tenant-scoped DB migration.
func (p *Plugin) Migration(version, name, upSQL, downSQL string) {
	p.migrations = append(p.migrations, Migration{Version: version, Name: name, UpSQL: upSQL, DownSQL: downSQL})
}

// Handle registers a Gin handler and records route metadata + OpenAPI docs.
func (p *Plugin) Handle(method, path string, h gin.HandlerFunc, docs ...option.OperationOption) {
	p.engine.Handle(method, path, h)
	p.routes = append(p.routes, route{Method: method, Path: path})
	sr := p.spec.Handle(method, path, func(*gin.Context) {})
	if len(docs) > 0 {
		sr.With(docs...)
	}
}

// GET registers a GET route (see Handle).
func (p *Plugin) GET(path string, h gin.HandlerFunc, d ...option.OperationOption) {
	p.Handle(http.MethodGet, path, h, d...)
}

// POST registers a POST route (see Handle).
func (p *Plugin) POST(path string, h gin.HandlerFunc, d ...option.OperationOption) {
	p.Handle(http.MethodPost, path, h, d...)
}

func (p *Plugin) buildManifest() map[string]any {
	routes := make([]route, len(p.routes))
	copy(routes, p.routes)
	for i := range routes {
		if p.public[routes[i].Path] {
			routes[i].Public = true
		}
	}
	specJSON, err := p.spec.MarshalJSON()
	if err != nil {
		log.Printf("[plugin] openapi gen failed: %v", err)
		specJSON = []byte("{}")
	}
	publicPaths := make([]string, 0, len(p.public))
	for pp := range p.public {
		publicPaths = append(publicPaths, pp)
	}
	return map[string]any{
		"name":         p.cfg.Name,
		"version":      p.cfg.Version,
		"plugin_type":  "internal",
		"routes":       routes,
		"public_paths": publicPaths,
		"migrations":   p.migrations,
		"openapi_spec": json.RawMessage(specJSON),
	}
}

// Run starts the HTTP server, registers with Core, runs heartbeat, blocks on ctx.
func (p *Plugin) Run(ctx context.Context) error {
	manifest := p.buildManifest()
	p.engine.GET("/_apicorex/manifest", func(c *gin.Context) { c.JSON(http.StatusOK, manifest) })
	p.engine.GET("/_apicorex/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })

	lis, err := net.Listen("tcp", p.cfg.PluginAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", p.cfg.PluginAddr, err)
	}
	baseURL := p.cfg.PluginBaseURL
	if baseURL == "" {
		baseURL = "http://" + lis.Addr().String()
	}
	srv := &http.Server{Handler: p.engine}

	g, gCtx := errgroup.WithContext(ctx)
	g.Go(func() error {
		log.Printf("[plugin] %s listening on %s (advertised %s)", p.cfg.Name, lis.Addr(), baseURL)
		if err := srv.Serve(lis); err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	})
	if err := p.register(gCtx, baseURL); err != nil {
		srv.Close()
		return fmt.Errorf("register with core: %w", err)
	}
	log.Printf("[plugin] %s registered as %s", p.cfg.Name, p.pluginID)
	g.Go(func() error { p.heartbeat(gCtx, baseURL); return nil })
	g.Go(func() error {
		<-gCtx.Done()
		p.deregister()
		sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(sc)
	})
	return g.Wait()
}

// register retries until Core accepts the registration or ctx is cancelled.
// Backoff grows from 1s up to a 30s cap so a plugin started before Core (or
// during a long Core outage) keeps trying instead of giving up.
func (p *Plugin) register(ctx context.Context, baseURL string) error {
	body, _ := json.Marshal(map[string]string{"base_url": baseURL, "api_key": p.cfg.APIKey})
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	attempt := 0
	for {
		attempt++
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.CoreURL+"/_core/register", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			var rr struct {
				PluginID    string `json:"plugin_id"`
				PluginToken string `json:"plugin_token"`
			}
			err = json.NewDecoder(resp.Body).Decode(&rr)
			resp.Body.Close()
			if err != nil {
				return err
			}
			p.pluginID = rr.PluginID
			p.pluginToken = rr.PluginToken
			return nil
		}
		// log occasionally so a stuck plugin is visible without spamming
		if err != nil {
			if attempt%5 == 1 {
				log.Printf("[plugin] %s waiting for core at %s: %v", p.cfg.Name, p.cfg.CoreURL, err)
			}
		} else {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			log.Printf("[plugin] %s register rejected (%d): %s", p.cfg.Name, resp.StatusCode, strings.TrimSpace(string(b)))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// heartbeat pings Core every 15s. If Core no longer recognizes this plugin
// (404 — e.g. Core restarted and lost its in-memory registry) or is unreachable
// for a while, the plugin re-registers so it self-heals without a restart.
func (p *Plugin) heartbeat(ctx context.Context, baseURL string) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	missed := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			status, err := p.sendHeartbeat(ctx)
			switch {
			case err == nil && status == http.StatusOK:
				missed = 0
			case status == http.StatusNotFound || status == http.StatusUnauthorized:
				// Core forgot us (restart) — re-register immediately.
				log.Printf("[plugin] %s heartbeat rejected (%d), re-registering", p.cfg.Name, status)
				p.reregister(ctx, baseURL)
				missed = 0
			default:
				// network error or 5xx — Core may be down/restarting.
				missed++
				if missed >= 3 {
					log.Printf("[plugin] %s missed %d heartbeats, re-registering", p.cfg.Name, missed)
					p.reregister(ctx, baseURL)
					missed = 0
				}
			}
		}
	}
}

func (p *Plugin) sendHeartbeat(ctx context.Context) (int, error) {
	body, _ := json.Marshal(map[string]string{"plugin_id": p.pluginID, "plugin_token": p.pluginToken})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.CoreURL+"/_core/heartbeat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// reregister re-runs registration (blocking with backoff) to get a fresh
// plugin_id/token. Safe because Core evicts stale same-name entries on register.
func (p *Plugin) reregister(ctx context.Context, baseURL string) {
	if err := p.register(ctx, baseURL); err != nil {
		log.Printf("[plugin] %s re-register stopped: %v", p.cfg.Name, err)
		return
	}
	log.Printf("[plugin] %s re-registered as %s", p.cfg.Name, p.pluginID)
}

func (p *Plugin) deregister() {
	body, _ := json.Marshal(map[string]string{"plugin_id": p.pluginID, "plugin_token": p.pluginToken})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.CoreURL+"/_core/deregister", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
}
