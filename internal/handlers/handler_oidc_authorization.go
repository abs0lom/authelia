package handlers

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ory/fosite"
	"github.com/ory/fosite/handler/openid"
	"github.com/ory/fosite/token/jwt"

	"github.com/authelia/authelia/v4/internal/logging"
	"github.com/authelia/authelia/v4/internal/middlewares"
	"github.com/authelia/authelia/v4/internal/oidc"
	"github.com/authelia/authelia/v4/internal/session"
)

func oidcAuthorization(ctx *middlewares.AutheliaCtx, rw http.ResponseWriter, r *http.Request) {
	ar, err := ctx.Providers.OpenIDConnect.Fosite.NewAuthorizeRequest(ctx, r)
	if err != nil {
		logging.Logger().Errorf("Error occurred in NewAuthorizeRequest: %+v", err)
		ctx.Providers.OpenIDConnect.Fosite.WriteAuthorizeError(rw, ar, err)

		return
	}

	clientID := ar.GetClient().GetID()
	client, err := ctx.Providers.OpenIDConnect.Store.GetInternalClient(clientID)

	if err != nil {
		err := fmt.Errorf("unable to find related client configuration with name '%s': %v", ar.GetID(), err)
		ctx.Logger.Error(err)
		ctx.Providers.OpenIDConnect.Fosite.WriteAuthorizeError(rw, ar, err)

		return
	}

	userSession := ctx.GetSession()

	requestedScopes := ar.GetRequestedScopes()
	requestedAudience := ar.GetRequestedAudience()

	isAuthInsufficient := !client.IsAuthenticationLevelSufficient(userSession.AuthenticationLevel)

	if isAuthInsufficient || (isConsentMissing(userSession.OIDCWorkflowSession, requestedScopes, requestedAudience)) {
		oidcAuthorizeHandleAuthorizationOrConsentInsufficient(ctx, userSession, client, isAuthInsufficient, rw, r, ar)

		return
	}

	extraClaims := oidcGrantRequests(ar, requestedScopes, requestedAudience, &userSession)

	workflowCreated := time.Unix(userSession.OIDCWorkflowSession.CreatedTimestamp, 0)

	userSession.OIDCWorkflowSession = nil
	if err := ctx.SaveSession(userSession); err != nil {
		ctx.Logger.Error(err)
		http.Error(rw, err.Error(), http.StatusInternalServerError)

		return
	}

	issuer, err := ctx.ExternalRootURL()
	if err != nil {
		ctx.Logger.Errorf("Error occurred obtaining issuer: %+v", err)
		ctx.Providers.OpenIDConnect.Fosite.WriteAuthorizeError(rw, ar, err)

		return
	}

	authTime, err := userSession.AuthenticatedTime(client.Policy)
	if err != nil {
		ctx.Logger.Errorf("Error occurred obtaining authentication timestamp: %+v", err)
		ctx.Providers.OpenIDConnect.Fosite.WriteAuthorizeError(rw, ar, err)

		return
	}

	response, err := ctx.Providers.OpenIDConnect.Fosite.NewAuthorizeResponse(ctx, ar, &oidc.OpenIDSession{
		DefaultSession: &openid.DefaultSession{
			Claims: &jwt.IDTokenClaims{
				Subject:     userSession.Username,
				Issuer:      issuer,
				AuthTime:    authTime,
				RequestedAt: workflowCreated,
				IssuedAt:    time.Now(),
				Nonce:       ar.GetRequestForm().Get("nonce"),
				Audience:    ar.GetGrantedAudience(),
				Extra:       extraClaims,
			},
			Headers: &jwt.Headers{Extra: map[string]interface{}{
				"kid": ctx.Providers.OpenIDConnect.KeyManager.GetActiveKeyID(),
			}},
			Subject:  userSession.Username,
			Username: userSession.Username,
		},
		ClientID: clientID,
	})
	if err != nil {
		ctx.Logger.Errorf("Error occurred in NewAuthorizeResponse: %+v", err)
		ctx.Providers.OpenIDConnect.Fosite.WriteAuthorizeError(rw, ar, err)

		return
	}

	ctx.Providers.OpenIDConnect.Fosite.WriteAuthorizeResponse(rw, ar, response)
}

func oidcAuthorizeHandleAuthorizationOrConsentInsufficient(
	ctx *middlewares.AutheliaCtx, userSession session.UserSession, client *oidc.InternalClient, isAuthInsufficient bool,
	rw http.ResponseWriter, r *http.Request,
	ar fosite.AuthorizeRequester) {
	issuer, err := ctx.ExternalRootURL()
	if err != nil {
		ctx.Logger.Error(err)
		http.Error(rw, err.Error(), http.StatusBadRequest)

		return
	}

	redirectURL := fmt.Sprintf("%s%s", issuer, string(ctx.Request.RequestURI()))

	ctx.Logger.Debugf("User %s must consent with scopes %s",
		userSession.Username, strings.Join(ar.GetRequestedScopes(), ", "))

	userSession.OIDCWorkflowSession = &session.OIDCWorkflowSession{
		ClientID:                   client.ID,
		RequestedScopes:            ar.GetRequestedScopes(),
		RequestedAudience:          ar.GetRequestedAudience(),
		AuthURI:                    redirectURL,
		TargetURI:                  ar.GetRedirectURI().String(),
		RequiredAuthorizationLevel: client.Policy,
		CreatedTimestamp:           time.Now().Unix(),
	}

	if err := ctx.SaveSession(userSession); err != nil {
		ctx.Logger.Errorf("Unable to save session: %v", err)
		http.Error(rw, err.Error(), http.StatusInternalServerError)

		return
	}

	if isAuthInsufficient {
		http.Redirect(rw, r, issuer, http.StatusFound)
	} else {
		http.Redirect(rw, r, fmt.Sprintf("%s/consent", issuer), http.StatusFound)
	}
}
