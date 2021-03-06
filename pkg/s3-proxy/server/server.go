package server

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
	"github.com/go-chi/httptracer"
	"github.com/oxyno-zeta/s3-proxy/pkg/s3-proxy/authx/authentication"
	"github.com/oxyno-zeta/s3-proxy/pkg/s3-proxy/authx/authorization"
	"github.com/oxyno-zeta/s3-proxy/pkg/s3-proxy/bucket"
	"github.com/oxyno-zeta/s3-proxy/pkg/s3-proxy/config"
	"github.com/oxyno-zeta/s3-proxy/pkg/s3-proxy/log"
	"github.com/oxyno-zeta/s3-proxy/pkg/s3-proxy/metrics"
	"github.com/oxyno-zeta/s3-proxy/pkg/s3-proxy/server/middlewares"
	"github.com/oxyno-zeta/s3-proxy/pkg/s3-proxy/server/utils"
	"github.com/oxyno-zeta/s3-proxy/pkg/s3-proxy/tracing"
	"github.com/oxyno-zeta/s3-proxy/pkg/s3-proxy/version"
	"github.com/thoas/go-funk"
)

type Server struct {
	logger     log.Logger
	cfgManager config.Manager
	metricsCl  metrics.Client
	server     *http.Server
	tracingSvc tracing.Service
}

func NewServer(logger log.Logger, cfgManager config.Manager, metricsCl metrics.Client, tracingSvc tracing.Service) *Server {
	return &Server{
		logger:     logger,
		cfgManager: cfgManager,
		metricsCl:  metricsCl,
		tracingSvc: tracingSvc,
	}
}

func (svr *Server) Listen() error {
	svr.logger.Infof("Server listening on %s", svr.server.Addr)
	err := svr.server.ListenAndServe()

	return err
}

func (svr *Server) GenerateServer() error {
	// Get configuration
	cfg := svr.cfgManager.GetConfig()
	// Generate router
	r, err := svr.generateRouter()
	if err != nil {
		return err
	}

	// Create server
	addr := cfg.Server.ListenAddr + ":" + strconv.Itoa(cfg.Server.Port)
	server := &http.Server{
		Addr:    addr,
		Handler: r,
	}

	// Prepare for configuration onChange
	svr.cfgManager.AddOnChangeHook(func() {
		// Generate router
		r, err2 := svr.generateRouter()
		if err2 != nil {
			svr.logger.Fatal(err2)
		}
		// Change server handler
		server.Handler = r
		svr.logger.Info("Server handler reloaded")
	})

	// Store server
	svr.server = server

	return nil
}

func (svr *Server) generateRouter() (http.Handler, error) {
	// Get configuration
	cfg := svr.cfgManager.GetConfig()

	// Create authentication service
	authenticationSvc := authentication.NewAuthenticationService(cfg, svr.metricsCl)

	// Create router
	r := chi.NewRouter()

	// A good base middleware stack
	r.Use(middleware.Compress(
		5,
		"text/html",
		"text/css",
		"text/plain",
		"text/javascript",
		"application/javascript",
		"application/x-javascript",
		"application/json",
		"application/atom+xml",
		"application/rss+xml",
		"image/svg+xml",
	))
	r.Use(middleware.NoCache)
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	// Manage tracing
	// Create http tracer configuration
	httptraCfg := httptracer.Config{
		ServiceName:    "s3-proxy",
		ServiceVersion: version.GetVersion().Version,
		SampleRate:     1,
		OperationName:  "http.request",
		Tags:           cfg.Tracing.FixedTags,
	}
	// Put tracing middlewares
	r.Use(httptracer.Tracer(svr.tracingSvc.GetTracer(), httptraCfg))
	r.Use(middlewares.ImproveTracing())
	r.Use(middlewares.NewStructuredLogger(svr.logger))
	r.Use(svr.metricsCl.Instrument("business"))

	// Check if auth if enabled and oidc enabled
	if cfg.AuthProviders != nil && cfg.AuthProviders.OIDC != nil {
		for _, v := range cfg.AuthProviders.OIDC {
			err := authenticationSvc.OIDCEndpoints(v, r)
			if err != nil {
				return nil, err
			}
		}
	}

	notFoundHandler := func(w http.ResponseWriter, r *http.Request) {
		// Get logger
		logger := middlewares.GetLogEntry(r)
		// Get request URI
		requestURI := r.URL.RequestURI()
		utils.HandleNotFound(logger, w, cfg.Templates, requestURI)
	}

	internalServerHandlerGen := func(err error) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			// Get logger
			logger := middlewares.GetLogEntry(r)
			// Get request URI
			requestURI := r.URL.RequestURI()
			utils.HandleInternalServerError(logger, w, cfg.Templates, requestURI, err)
		}
	}

	// Create host router
	hr := NewHostRouter(notFoundHandler, internalServerHandlerGen)

	// Load main route only if main bucket path support option isn't enabled
	if cfg.ListTargets.Enabled {
		// Create new router
		rt := chi.NewRouter()
		// Make list of resources from resource
		resources := make([]*config.Resource, 0)
		if cfg.ListTargets.Resource != nil {
			resources = append(resources, cfg.ListTargets.Resource)
		}
		// Manage path for list targets feature
		// Loop over path list
		funk.ForEach(cfg.ListTargets.Mount.Path, func(path string) {
			rt.Route(path, func(rt2 chi.Router) {
				// Add authentication middleware to router
				rt2 = rt2.With(authenticationSvc.Middleware(resources))

				// Add authorization middleware to router
				rt2 = rt2.With(authorization.Middleware(cfg, svr.metricsCl))

				rt2.Get("/", func(rw http.ResponseWriter, req *http.Request) {
					logEntry := middlewares.GetLogEntry(req)
					generateTargetList(rw, req.RequestURI, logEntry, cfg)
				})
			})
		})
		// Create domain
		domain := cfg.ListTargets.Mount.Host
		if domain == "" {
			domain = "*"
		}
		// Mount domain from configuration
		hr.Map(domain, rt)
	}

	// Load all targets routes
	funk.ForEach(cfg.Targets, func(tgt *config.TargetConfig) {
		// Manage domain
		domain := tgt.Mount.Host
		if domain == "" {
			domain = "*"
		}
		// Get router from hostrouter if exists
		rt := hr.Get(domain)
		if rt == nil {
			// Create a new router
			rt = chi.NewRouter()
		}
		// Loop over path list
		funk.ForEach(tgt.Mount.Path, func(path string) {
			rt.Route(path, func(rt2 chi.Router) {
				// Add Bucket request context middleware to initialize it
				rt2.Use(middlewares.BucketRequestContext(tgt, cfg.Templates, path, svr.metricsCl))

				// Add authentication middleware to router
				rt2.Use(authenticationSvc.Middleware(tgt.Resources))

				// Add authorization middleware to router
				rt2.Use(authorization.Middleware(cfg, svr.metricsCl))

				// Check if GET action is enabled
				if tgt.Actions.GET != nil && tgt.Actions.GET.Enabled {
					// Add GET method to router
					rt2.Get("/*", func(rw http.ResponseWriter, req *http.Request) {
						// Get bucket request context
						brctx := middlewares.GetBucketRequestContext(req)
						// Get request path
						requestPath := chi.URLParam(req, "*")
						// Proxy GET Request
						brctx.Get(requestPath)
					})
				}

				// Check if PUT action is enabled
				if tgt.Actions.PUT != nil && tgt.Actions.PUT.Enabled {
					// Add PUT method to router
					rt2.Put("/*", func(rw http.ResponseWriter, req *http.Request) {
						// Get bucket request context
						brctx := middlewares.GetBucketRequestContext(req)
						// Get request path
						requestPath := chi.URLParam(req, "*")
						// Get logger
						logEntry := middlewares.GetLogEntry(req)
						if err := req.ParseForm(); err != nil {
							logEntry.Error(err)
							brctx.HandleInternalServerError(err, path)
							return
						}
						// Parse multipart form
						err := req.ParseMultipartForm(0)
						if err != nil {
							logEntry.Error(err)
							brctx.HandleInternalServerError(err, path)
							return
						}
						// Get file from form
						file, fileHeader, err := req.FormFile("file")
						if err != nil {
							logEntry.Error(err)
							brctx.HandleInternalServerError(err, path)
							return
						}
						// Create input for put request
						inp := &bucket.PutInput{
							RequestPath: requestPath,
							Filename:    fileHeader.Filename,
							Body:        file,
							ContentType: fileHeader.Header.Get("Content-Type"),
							ContentSize: fileHeader.Size,
						}
						brctx.Put(inp)
					})
				}

				// Check if DELETE action is enabled
				if tgt.Actions.DELETE != nil && tgt.Actions.DELETE.Enabled {
					// Add DELETE method to router
					rt2.Delete("/*", func(rw http.ResponseWriter, req *http.Request) {
						// Get bucket request context
						brctx := middlewares.GetBucketRequestContext(req)
						// Get request path
						requestPath := chi.URLParam(req, "*")
						// Proxy GET Request
						brctx.Delete(requestPath)
					})
				}
			})
		})
		// Mount domain from target
		hr.Map(domain, rt)
	})

	// Mount host router
	r.Mount("/", hr)

	return r, nil
}

func generateTargetList(rw http.ResponseWriter, path string, logger log.Logger, cfg *config.Config) {
	err := utils.TemplateExecution(cfg.Templates.TargetList, "", logger, rw, struct{ Targets []*config.TargetConfig }{Targets: cfg.Targets}, 200)
	if err != nil {
		logger.Error(err)
		// ! In this case, use default default local files for error
		utils.HandleInternalServerError(logger, rw, cfg.Templates, path, err)
		// Stop here
		return
	}
}
