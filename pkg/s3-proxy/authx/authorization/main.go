package authorization

import (
	"errors"
	"net/http"

	"github.com/oxyno-zeta/s3-proxy/pkg/s3-proxy/authx/authentication"
	"github.com/oxyno-zeta/s3-proxy/pkg/s3-proxy/authx/models"
	"github.com/oxyno-zeta/s3-proxy/pkg/s3-proxy/config"
	"github.com/oxyno-zeta/s3-proxy/pkg/s3-proxy/server/middlewares"
	"github.com/oxyno-zeta/s3-proxy/pkg/s3-proxy/server/utils"
)

var errAuthorizationMiddlewareNotSupported = errors.New("authorization not supported")

func AuthorizationMiddleware(cfg *config.Config, tplConfig *config.TemplateConfig) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			logger := middlewares.GetLogEntry(r)

			// Get request resource from request
			resource := authentication.GetRequestResource(r)
			// Check if resource exists
			if resource == nil {
				// Resource doesn't exists
				// In this case, authentication is skipped, need to skip authorization too
				logger.Debug("no resource found in authorization, means that authentication was skipped => skip authorization too")
				next.ServeHTTP(w, r)
				return
			}

			// Check if resource is whitelisted
			if resource.WhiteList != nil && *resource.WhiteList {
				// Resource is whitelisted
				logger.Debug("authorization skipped because resource is whitelisted")
				next.ServeHTTP(w, r)
				return
			}

			// Get user from context
			user := authentication.GetAuthenticatedUser(r)

			// Check if resource is basic authentication
			if resource.Basic != nil {
				// Case user in basic auth user
				buser := user.(*models.BasicAuthUser)
				// Resource is basic authenticated
				logger.Debug("authorization for basic authentication => nothing needed")
				logger.Info("Basic auth user %s authorized", buser.Username)
				next.ServeHTTP(w, r)
				return
			}

			// Get request data
			requestURI := r.URL.RequestURI()
			// httpMethod := r.Method

			// Get bucket request context
			brctx := middlewares.GetBucketRequestContext(r)

			// Check if resource is OIDC
			if resource.OIDC != nil {
				// Cast user in oidc user
				ouser := user.(*models.OIDCUser)
				// Check if authorized
				if !isAuthorized(ouser.Groups, ouser.Email, resource.OIDC.AuthorizationAccesses) {
					logger.Errorf("Forbidden user %s", ouser.Email)
					// Check if bucket request context doesn't exist to use local default files
					if brctx == nil {
						utils.HandleForbidden(logger, w, tplConfig, requestURI)
					} else {
						brctx.HandleForbidden(requestURI)
					}
					return
				}
				logger.Info("OIDC user %s authorized", ouser.Email)
				next.ServeHTTP(w, r)
				return
			}

			// Error, this case shouldn't arrive
			err := errAuthorizationMiddlewareNotSupported
			logger.Error(err)
			// Check if bucket request context doesn't exist to use local default files
			if brctx == nil {
				utils.HandleInternalServerError(logger, w, cfg.Templates, requestURI, err)
			} else {
				brctx.HandleInternalServerError(err, requestURI)
			}
		})
	}
}