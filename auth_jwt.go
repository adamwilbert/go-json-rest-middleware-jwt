// Package jwt provides Json-Web-Token authentication for the go-json-rest framework
package jwt

import (
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/ant0ine/go-json-rest/rest"
	"github.com/dgrijalva/jwt-go"
)

// JWTMiddleware provides a Json-Web-Token authentication implementation. On failure, a 401 HTTP response
// is returned. On success, the wrapped middleware is called, and the userId is made available as
// r.Env["REMOTE_USER"].(string).
// Users can get a token by posting a json request to LoginHandler. The token then needs to be passed in
// the Authentication header. Example: Authorization:Bearer XXX_TOKEN_XXX
type JWTMiddleware struct {
	// Realm name to display to the user. Required.
	Realm string

	// signing algorithm - possible values are HS256, HS384, HS512
	// Optional, default is HS256.
	SigningAlgorithm string

	// Secret key used for signing. Required.
	Key []byte

	// Duration that a jwt token is valid. Optional, defaults to one hour.
	Timeout time.Duration

	// This field allows clients to refresh their token until MaxRefresh has passed.
	// Note that clients can refresh their token in the last moment of MaxRefresh.
	// This means that the maximum validity timespan for a token is MaxRefresh + Timeout.
	// Optional, defaults to 0 meaning not refreshable.
	MaxRefresh time.Duration

	// Callback function that should perform the authentication of the user based on userId and
	// password. Must return true on success, false on failure. Required.
	Authenticator func(userId string, password string) bool

	// Callback function that should perform the authorization of the authenticated user. Called
	// only after an authentication success. Must return true on success, false on failure.
	// Optional, default to success.
	Authorizator func(userId string, r *rest.Request) bool

	// Callback function that will be called during login.
	// Using this function it is possible to add additional payload data to the webtoken.
	// The data is then made available during requests via request.Env["JWT_PAYLOAD"].
	// Note that the payload is not encrypted.
	// The attributes mentioned on jwt.io can't be used as keys for the map.
	// Optional, by default no additional data will be set.
	PayloadFunc func(userId string) map[string]interface{}
}

// MiddlewareFunc makes JWTMiddleware implement the Middleware interface.
func (mw *JWTMiddleware) MiddlewareFunc(handler rest.HandlerFunc) rest.HandlerFunc {

	if mw.Realm == "" {
		log.Fatal("Realm is required")
	}
	if mw.SigningAlgorithm == "" {
		mw.SigningAlgorithm = "HS256"
	}
	if mw.Key == nil {
		log.Fatal("Key required")
	}
	if mw.Timeout == 0 {
		mw.Timeout = time.Hour
	}
	if mw.Authenticator == nil {
		log.Fatal("Authenticator is required")
	}
	if mw.Authorizator == nil {
		mw.Authorizator = func(userId string, r *rest.Request) bool {
			return true
		}
	}

	return func(w rest.ResponseWriter, r *rest.Request) { mw.middlewareImpl(w, r, handler) }
}

func (mw *JWTMiddleware) middlewareImpl(w rest.ResponseWriter, r *rest.Request, handler rest.HandlerFunc) {
	token, err := mw.parseToken(r)

	if err != nil {
		mw.unauthorized(w, err.Error())
		return
	}

	id := token.Claims["id"].(string)

	r.Env["REMOTE_USER"] = id
	r.Env["JWT_PAYLOAD"] = token.Claims

	if !mw.Authorizator(id, r) {
		mw.unauthorized(w, "Permission Denied")
		return
	}

	handler(w, r)
}

// ExtractClaims allows to retrieve the payload
func ExtractClaims(r *rest.Request) map[string]interface{} {
	if r.Env["JWT_PAYLOAD"] == nil {
		emptyClaims := make(map[string]interface{})
		return emptyClaims
	}
	jwtClaims := r.Env["JWT_PAYLOAD"].(map[string]interface{})
	return jwtClaims
}

type resultToken struct {
	Token string `json:"token"`
}

type login struct {
	Username string `json:"email"`
	Password string `json:"password"`
}

// LoginHandler can be used by clients to get a jwt token.
// Payload needs to be json in the form of {"username": "USERNAME", "password": "PASSWORD"}.
// Reply will be of the form {"token": "TOKEN"}.
func (mw *JWTMiddleware) LoginHandler(w rest.ResponseWriter, r *rest.Request) {
	loginVals := login{}
	err := r.DecodeJsonPayload(&loginVals)

	if err != nil {
		mw.unauthorized(w, "Error Reading Login Values")
		return
	}

	if !mw.Authenticator(loginVals.Username, loginVals.Password) {
		mw.unauthorized(w, "Not Authenticated")
		return
	}

	token := jwt.New(jwt.GetSigningMethod(mw.SigningAlgorithm))

	if mw.PayloadFunc != nil {
		for key, value := range mw.PayloadFunc(loginVals.Username) {
			token.Claims[key] = value
		}
	}

	token.Claims["id"] = loginVals.Username
	token.Claims["exp"] = time.Now().Add(mw.Timeout).Unix()
	if mw.MaxRefresh != 0 {
		token.Claims["orig_iat"] = time.Now().Unix()
	}
	tokenString, err := token.SignedString(mw.Key)

	if err != nil {
		mw.unauthorized(w, "Error creating token")
		return
	}
	type responseStruct struct {
		Token string `json:"token"`
		ID    interface{}
	}

	w.WriteJson(responseStruct{Token: tokenString, ID: token.Claims["userid"]})
}

func (mw *JWTMiddleware) parseToken(r *rest.Request) (*jwt.Token, error) {
	authHeader := r.Header.Get("Authorization")

	if authHeader == "" {
		return nil, errors.New("Auth header empty")
	}

	parts := strings.SplitN(authHeader, " ", 2)
	if !(len(parts) == 2 && parts[0] == "Bearer") {
		return nil, errors.New("Invalid auth header")
	}

	return jwt.Parse(parts[1], func(token *jwt.Token) (interface{}, error) {
		if jwt.GetSigningMethod(mw.SigningAlgorithm) != token.Method {
			return nil, errors.New("Invalid signing algorithm")
		}
		return mw.Key, nil
	})
}

// RefreshHandler can be used to refresh a token. The token still needs to be valid on refresh.
// Shall be put under an endpoint that is using the JWTMiddleware.
// Reply will be of the form {"token": "TOKEN"}.
func (mw *JWTMiddleware) RefreshHandler(w rest.ResponseWriter, r *rest.Request) {
	token, err := mw.parseToken(r)

	// Token should be valid anyway as the RefreshHandler is authed
	if err != nil {
		mw.unauthorized(w, err.Error())
		return
	}

	origIat := int64(token.Claims["orig_iat"].(float64))

	if origIat < time.Now().Add(-mw.MaxRefresh).Unix() {
		mw.unauthorized(w, "Error Creating Token")
		return
	}

	newToken := jwt.New(jwt.GetSigningMethod(mw.SigningAlgorithm))

	for key := range token.Claims {
		newToken.Claims[key] = token.Claims[key]
	}

	newToken.Claims["id"] = token.Claims["id"]
	newToken.Claims["exp"] = time.Now().Add(mw.Timeout).Unix()
	newToken.Claims["orig_iat"] = origIat
	tokenString, err := newToken.SignedString(mw.Key)

	if err != nil {
		mw.unauthorized(w, "Error Creating Token")
		return
	}

	w.WriteJson(resultToken{Token: tokenString})
}

func (mw *JWTMiddleware) unauthorized(w rest.ResponseWriter, status string) {
	w.Header().Set("WWW-Authenticate", "JWT realm="+mw.Realm)
	rest.Error(w, status, http.StatusUnauthorized)
}
