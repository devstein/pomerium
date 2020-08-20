package forwardauth

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/gorilla/mux"

	"github.com/pomerium/pomerium/internal/httputil"
	"github.com/pomerium/pomerium/internal/sessions"
	"github.com/pomerium/pomerium/internal/urlutil"
)

// Router is the interface used to setup handlers related to forward auth requests flow.
type Router interface {
	// Init initializes handlers associated with the mux router.
	// Theses handlers will handle the forward auth requests flow.
	Init(*ForwardAuth, *mux.Router)
}

var registerHandler = map[string]Router{
	ForwardingProxyNginx:   new(nginx),
	ForwardingProxyTraefik: new(traefik),
}

// registerFwdAuthHandlers returns a set of handlers that support using pomerium
// as a "forward-auth" provider with other reverse proxies like nginx, traefik.
//
// see : https://www.pomerium.io/configuration/#forward-auth
func (fa *ForwardAuth) registerFwdAuthHandlers() http.Handler {
	r := httputil.NewRouter()
	r.StrictSlash(true)
	r.Use(func(h http.Handler) http.Handler {
		return sessions.RetrieveSession(fa.state.Load().sessionStore)(h)
	})
	r.Use(fa.jwtClaimMiddleware)

	proxyType := fa.state.Load().proxyType
	registerHandler[proxyType].Init(fa, r)
	return r
}

// Verify checks a user's credentials for an arbitrary host. If the user
// is properly authenticated and is authorized to access the supplied host,
// a `200` http status code is returned. If the user is not authenticated, they
// will be redirected to the authenticate service to sign in with their identity
// provider. If the user is unauthorized, a `401` error is returned.
func (fa *ForwardAuth) Verify(verifyOnly bool) http.Handler {
	return httputil.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		state := fa.state.Load()

		var err error
		if status := r.FormValue("auth_status"); status == fmt.Sprint(http.StatusForbidden) {
			return httputil.NewError(http.StatusForbidden, errors.New(http.StatusText(http.StatusForbidden)))
		}

		uri, err := getURIStringFromRequest(r)
		if err != nil {
			return httputil.NewError(http.StatusBadRequest, err)
		}

		ar, err := fa.isAuthorized(w, r)
		if err != nil {
			return httputil.NewError(http.StatusBadRequest, err)
		}

		if ar.authorized {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "Access to %s is allowed.", uri.Host)
			return nil
		}

		unAuthenticated := ar.statusCode == http.StatusUnauthorized
		if unAuthenticated {
			state.sessionStore.ClearSession(w, r)
		}

		_, err = sessions.FromContext(r.Context())
		hasSession := err == nil
		if hasSession && !unAuthenticated {
			return httputil.NewError(http.StatusForbidden, errors.New("access denied"))
		}

		if verifyOnly {
			return httputil.NewError(http.StatusUnauthorized, err)
		}

		fa.redirectToSignInWithURI(w, r, uri)
		return nil
	})
}

// redirectToSignInWithURI redirects request to authenticate signin url,
// with all necessary information extracted from given input uri.
func (fa *ForwardAuth) redirectToSignInWithURI(w http.ResponseWriter, r *http.Request, uri *url.URL) {
	state := fa.state.Load()

	// Traefik set the uri in the header, we must set it in redirect uri if present. Otherwise, request like
	// https://example.com/foo will be redirected to https://example.com after authentication.
	if xfu := r.Header.Get(httputil.HeaderForwardedURI); xfu != "/" {
		uri.Path = xfu
	}

	// redirect to authenticate
	authN := *state.authenticateSigninURL
	q := authN.Query()
	q.Set(urlutil.QueryCallbackURI, uri.String())
	q.Set(urlutil.QueryRedirectURI, uri.String())              // final destination
	q.Set(urlutil.QueryForwardAuth, urlutil.StripPort(r.Host)) // add fwd auth to trusted audience
	authN.RawQuery = q.Encode()
	httputil.Redirect(w, r, urlutil.NewSignedURL(state.sharedKey, &authN).String(), http.StatusFound)
}

func getURIStringFromRequest(r *http.Request) (*url.URL, error) {
	// the route to validate will be pulled from the uri queryparam
	// or inferred from forwarding headers
	uriString := r.FormValue("uri")
	if uriString == "" {
		if r.Header.Get(httputil.HeaderForwardedProto) == "" || r.Header.Get(httputil.HeaderForwardedHost) == "" {
			return nil, errors.New("no uri to validate")
		}
		uriString = r.Header.Get(httputil.HeaderForwardedProto) + "://" +
			r.Header.Get(httputil.HeaderForwardedHost) +
			r.Header.Get(httputil.HeaderForwardedURI)
	}

	uri, err := urlutil.ParseAndValidateURL(uriString)
	if err != nil {
		return nil, err
	}
	return uri, nil
}