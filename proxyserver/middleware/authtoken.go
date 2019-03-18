//  Copyright (c) 2017 Rackspace
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
//  implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

package middleware

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/RocFang/hummingbird/client"
	"github.com/RocFang/hummingbird/common"
	"github.com/RocFang/hummingbird/common/conf"
	"github.com/RocFang/hummingbird/common/srv"
	"github.com/RocFang/hummingbird/common/tracing"
	"github.com/uber-go/tally"
	"go.uber.org/zap"
)

type identity struct {
	client          common.HTTPClient
	authURL         string
	authPlugin      string
	projectDomainID string
	userDomainID    string
	projectName     string
	userName        string
	password        string
	userAgent       string
}

type authToken struct {
	*identity
	next           http.Handler
	cacheDur       time.Duration
	preValidateDur time.Duration
	preValidations map[string]bool
	lock           sync.Mutex
}

var authHeaders = []string{"X-Identity-Status",
	"X-Service-Identity-Status",
	"X-Domain-Id",
	"X-Domain-Name",
	"X-Project-Id",
	"X-Project-Name",
	"X-Project-Domain-Id",
	"X-Project-Domain-Name",
	"X-User-Id",
	"X-User-Name",
	"X-User-Domain-Id",
	"X-User-Domain-Name",
	"X-Roles",
	"X-Service-Domain-Id",
	"X-Service-Domain-Name",
	"X-Service-Project-Id",
	"X-Service-Project-Name",
	"X-Service-Project-Domain-Id",
	"X-Service-Project-Domain-Name",
	"X-Service-User-Id",
	"X-Service-User-Name",
	"X-Service-User-Domain-Id",
	"X-Service-User-Domain-Name",
	"X-Service-Roles",
	"X-Service-Catalog",
	"X-Is-Admin-Project",
	//Deprecated Headers
	"X-Role",
	"X-User",
	"X-Tenant-Id",
	"X-Tenant-Name",
	"X-Tenant",
}

type domain struct {
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Enabled bool   `json:"enabled,omitempty"`
}

type project struct {
	ID      string  `json:"id,omitempty"`
	Name    string  `json:"name,omitempty"`
	Enabled bool    `json:"enabled,omitempty"`
	Domain  *domain `json:"domain"`
}

type token struct {
	ExpiresAt     time.Time `json:"expires_at"`
	MemcacheTtlAt time.Time
	IssuedAt      time.Time `json:"issued_at"`
	Methods       []string
	User          struct {
		ID      string
		Name    string
		Email   string
		Enabled bool
		Domain  struct {
			ID   string
			Name string
		}
	}
	Project *project
	Domain  *domain
	Roles   *[]struct {
		ID   string
		Name string
	}
	S3Creds *s3Blob
}

type s3Token struct {
	Access    string `json:"access"`
	Token     string `json:"token"`
	Signature string `json:"signature"`
}

type s3Creds struct {
	Credentials s3Token `json:"credentials"`
}

func (t token) Valid() bool {
	now := time.Now().Unix()
	return now < t.ExpiresAt.Unix()
}

func (t token) populateReqHeader(r *http.Request, headerPrefix string) {
	r.Header.Set(fmt.Sprintf("X%s-User-Id", headerPrefix), t.User.ID)
	r.Header.Set(fmt.Sprintf("X%s-User-Name", headerPrefix), t.User.Name)
	r.Header.Set(fmt.Sprintf("X%s-User-Domain-Id", headerPrefix), t.User.Domain.ID)
	r.Header.Set(fmt.Sprintf("X%s-User-Domain-Name", headerPrefix), t.User.Domain.Name)

	if project := t.Project; project != nil {
		r.Header.Set(fmt.Sprintf("X%s-Project-Name", headerPrefix), project.Name)
		r.Header.Set(fmt.Sprintf("X%s-Project-Id", headerPrefix), project.ID)
		r.Header.Set(fmt.Sprintf("X%s-Project-Domain-Name", headerPrefix), project.Domain.Name)
		r.Header.Set(fmt.Sprintf("X%s-Project-Domain-Id", headerPrefix), project.Domain.ID)
	}

	if domain := t.Domain; domain != nil {
		r.Header.Set(fmt.Sprintf("X%s-Domain-Id", headerPrefix), domain.ID)
		r.Header.Set(fmt.Sprintf("X%s-Domain-Name", headerPrefix), domain.Name)
	}

	if roles := t.Roles; roles != nil {
		roleNames := []string{}
		for _, role := range *t.Roles {
			roleNames = append(roleNames, role.Name)
		}
		r.Header.Set(fmt.Sprintf("X%s-Roles", headerPrefix), strings.Join(roleNames, ","))
	}
}

type identityReq struct {
	Auth struct {
		Identity struct {
			Methods  []string `json:"methods"`
			Password struct {
				User struct {
					Domain struct {
						ID string `json:"id"`
					} `json:"domain"`
					Name     string `json:"name"`
					Password string `json:"password"`
				} `json:"user"`
			} `json:"password"`
		} `json:"identity"`

		Scope struct {
			Project *project `json:"project"`
		} `json:"scope"`
	} `json:"auth"`
}

type identityResponse struct {
	Error *struct {
		Code    int
		Message string
		Title   string
	}
	Token *token
}

type credential struct {
	UserId    string `json:"user_id"`
	ProjectId string `json:"project_id"`
	Blob      string `json:"blob"`
	Id        string `json:"id"`
}

type s3Blob struct {
	Access string `json:"access"`
	Secret string `json:"secret"`
}

type credentialsResponse struct {
	Credentials []credential `json:"credentials"`
}

func (at *authToken) preValidate(ctx context.Context, proxyCtx *ProxyContext, authToken string) {
	at.lock.Lock()
	defer at.lock.Unlock()
	_, ok := at.preValidations[authToken]
	if ok {
		return
	} else {
		at.preValidations[authToken] = true
	}
	go func() {
		ctx = tracing.CopySpanFromContext(ctx)
		at.validate(ctx, proxyCtx, authToken)
		at.lock.Lock()
		defer at.lock.Unlock()
		delete(at.preValidations, authToken)
	}()
}

func (at *authToken) fetchAndValidateToken(ctx context.Context, proxyCtx *ProxyContext, authToken string) (*token, bool, error) {
	if proxyCtx == nil {
		return nil, false, errors.New("no proxyCtx")
	}
	cachedToken := at.loadTokenFromCache(ctx, proxyCtx, authToken)
	if cachedToken != nil {
		return cachedToken, true, nil
	}
	return at.validate(ctx, proxyCtx, authToken)
}

func (at *authToken) loadTokenFromCache(ctx context.Context, proxyCtx *ProxyContext, key string) *token {
	var cachedToken token
	if err := proxyCtx.Cache.GetStructured(ctx, key, &cachedToken); err == nil {
		if at.preValidateDur > 0 && !cachedToken.MemcacheTtlAt.IsZero() {
			invalidateEarlyTime := time.Now().Add(at.preValidateDur)
			if cachedToken.MemcacheTtlAt.Before(invalidateEarlyTime) {
				at.preValidate(ctx, proxyCtx, key)
			}
		}
		proxyCtx.Logger.Debug("Found cache token",
			zap.String("token", key))
		return &cachedToken
	} else {
		return nil
	}
}

func (at *authToken) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	proxyCtx := GetProxyContext(r)
	if proxyCtx.Authorize != nil {
		at.next.ServeHTTP(w, r)
		return
	}
	removeAuthHeaders(r)
	r.Header.Set("X-Identity-Status", "Invalid")
	serviceAuthToken := r.Header.Get("X-Service-Token")
	if serviceAuthToken != "" {
		serviceToken, serviceTokenValid, err := at.fetchAndValidateToken(r.Context(), proxyCtx, serviceAuthToken)
		if err != nil {
			srv.SimpleErrorResponse(w, http.StatusInternalServerError, "")
			return
		}
		if serviceToken != nil && serviceTokenValid {
			r.Header.Set("X-Service-Identity-Status", "Confirmed")
			serviceToken.populateReqHeader(r, "-Service")
		} else {
			r.Header.Set("X-Service-Identity-Status", "Invalid")
		}
	}
	if proxyCtx.S3Auth != nil {
		// Handle S3 auth validation first
		userToken, userTokenValid := at.validateS3Signature(r.Context(), proxyCtx)
		if userToken != nil && userTokenValid {
			r.Header.Set("X-Identity-Status", "Confirmed")
			userToken.populateReqHeader(r, "")
		} else {
			proxyCtx.Authorize = func(r *http.Request) (bool, int) {
				return false, http.StatusForbidden
			}
		}
	}

	userAuthToken := r.Header.Get("X-Auth-Token")
	if userAuthToken == "" {
		userAuthToken = r.Header.Get("X-Storage-Token")
	}
	if userAuthToken != "" {
		userToken, userTokenValid, err := at.fetchAndValidateToken(r.Context(), proxyCtx, userAuthToken)
		if err != nil {
			srv.SimpleErrorResponse(w, http.StatusInternalServerError, "")
			return
		}
		if userToken != nil && userTokenValid {
			r.Header.Set("X-Identity-Status", "Confirmed")
			userToken.populateReqHeader(r, "")
		}
	}
	at.next.ServeHTTP(w, r)
}

func (at *authToken) validateS3Signature(ctx context.Context, proxyCtx *ProxyContext) (*token, bool) {
	// Check for a cached token
	cachedToken := at.loadTokenFromCache(ctx, proxyCtx, "S3:"+proxyCtx.S3Auth.Key)
	if cachedToken != nil {
		proxyCtx.S3Auth.Account = cachedToken.Project.ID
		return cachedToken, proxyCtx.S3Auth.validateSignature([]byte(cachedToken.S3Creds.Secret))
	}
	tok, err := at.doValidateS3(ctx, proxyCtx, proxyCtx.S3Auth.StringToSign, proxyCtx.S3Auth.Key, proxyCtx.S3Auth.Signature)
	if err != nil {
		proxyCtx.Logger.Debug("Failed to validate s3 signature", zap.Error(err))
		return nil, false
	}

	if tok != nil {
		proxyCtx.S3Auth.Account = tok.Project.ID
		// TODO: We need to get and cache the secret to sign our own requests
		at.cacheToken(ctx, proxyCtx, "S3:"+proxyCtx.S3Auth.Key, tok)
		return tok, true
	}

	return nil, false
}

func (at *authToken) validate(ctx context.Context, proxyCtx *ProxyContext, authToken string) (*token, bool, error) {
	tok, err := at.doValidate(ctx, proxyCtx, authToken)
	if err != nil {
		proxyCtx.Logger.Debug("Failed to validate token", zap.Error(err))
		return nil, false, err
	}

	if tok != nil {
		at.cacheToken(ctx, proxyCtx, authToken, tok)
		return tok, true, nil
	}

	return nil, false, nil
}

func (at *authToken) cacheToken(ctx context.Context, proxyCtx *ProxyContext, key string, tok *token) {
	ttl := at.cacheDur
	if expiresIn := tok.ExpiresAt.Sub(time.Now()); expiresIn < ttl && expiresIn > 0 {
		ttl = expiresIn
	}
	tok.MemcacheTtlAt = time.Now().Add(ttl)
	proxyCtx.Cache.Set(ctx, key, *tok, int(ttl/time.Second))
}

// doValidateS3 returns an error for any problems attempting the validation
// (i.e. the end user did nothing wrong); it will return nil, nil if the user's
// credentials could not be validated; or it will return the token, nil on
// successful validation.
func (at *authToken) doValidateS3(ctx context.Context, proxyCtx *ProxyContext, stringToSign, key, signature string) (*token, error) {
	creds := &s3Creds{}
	creds.Credentials.Access = key
	creds.Credentials.Signature = signature
	creds.Credentials.Token = base64.URLEncoding.EncodeToString([]byte(stringToSign))
	credsReqBody, err := json.Marshal(creds)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", at.authURL+"v3/s3tokens", bytes.NewBuffer(credsReqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	r, err := at.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()

	if r.StatusCode >= 400 {
		return nil, errors.New(r.Status)
	}

	token, err := at.parseAuthResponse(r)
	if err != nil {
		return nil, err
	}
	if token == nil {
		return nil, nil
	}

	// Now we need to get the creds so that we can do the signing next time
	for tries := 0; tries < 2; tries++ { // second try will use fresh serverAuthToken
		var req2 *http.Request
		req2, err = http.NewRequest("GET", at.authURL+"v3/credentials?type=ec2&user_id="+token.User.ID, nil)
		if err != nil {
			return nil, err
		}
		var serverAuthToken string
		serverAuthToken, err = at.serverAuth(ctx, proxyCtx, tries > 0)
		if err != nil {
			return nil, err
		}
		req2.Header.Set("X-Auth-Token", serverAuthToken)
		req2.Header.Set("Content-Type", "application/json")
		var r2 *http.Response
		r2, err = at.client.Do(req2)
		if err != nil {
			return nil, err
		}
		defer r2.Body.Close() // yes, defer in loop, but loop is just 2 iterations at most
		if r2.StatusCode == 401 {
			err = errors.New("serverAuth was invalid, 401")
			continue
		}
		var s3creds *s3Blob
		s3creds, err = at.parseCredentialsResponse(r2, key)
		token.S3Creds = s3creds
		break
	}

	return token, err
}

// doValidate returns an error for any problems attempting the validation (i.e.
// the end user did nothing wrong); it will return nil, nil if the user's
// credentials could not be validated; or it will return the token, nil on
// successful validation.
func (at *authToken) doValidate(ctx context.Context, proxyCtx *ProxyContext, tken string) (*token, error) {
	if !strings.HasSuffix(at.authURL, "/") {
		at.authURL += "/"
	}
	var tok *token
	var err error
	for tries := 0; tries < 2; tries++ { // second try will use fresh serverAuthToken
		var req *http.Request
		req, err = http.NewRequest("GET", at.authURL+"v3/auth/tokens?nocatalog", nil)
		if err != nil {
			return nil, err
		}
		var serverAuthToken string
		serverAuthToken, err = at.serverAuth(ctx, proxyCtx, tries > 0)
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-Auth-Token", serverAuthToken)
		req.Header.Set("X-Subject-Token", tken)
		req.Header.Set("User-Agent", at.userAgent)
		req = req.WithContext(ctx)
		var resp *http.Response
		resp, err = at.client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close() // yes, defer in loop, but loop is just 2 iterations at most
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			err = fmt.Errorf("serverAuth was invalid, %d", resp.StatusCode)
			continue
		}
		if resp.StatusCode == 404 {
			return nil, nil
		}
		tok, err = at.parseAuthResponse(resp)
		break
	}
	return tok, err
}

// parseAuthResponse returns an error for any problems attempting the
// validation (i.e. the end user did nothing wrong); it will return nil, nil if
// the user's credentials could not be validated; or it will return the token,
// nil on successful validation.
func (at *authToken) parseAuthResponse(r *http.Response) (*token, error) {
	var resp identityResponse
	if err := json.NewDecoder(r.Body).Decode(&resp); err != nil {
		return nil, err
	}

	if e := resp.Error; e != nil {
		return nil, fmt.Errorf("%s : %s", r.Status, e.Message)
	}
	if r.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", r.Status)
	}
	if resp.Token == nil {
		return nil, errors.New("Response didn't contain token context")
	}
	if !resp.Token.Valid() {
		return nil, nil

	}
	return resp.Token, nil
}

func (at *authToken) parseCredentialsResponse(r *http.Response, key string) (*s3Blob, error) {
	var resp credentialsResponse
	if err := json.NewDecoder(r.Body).Decode(&resp); err != nil {
		return nil, err
	}
	if len(resp.Credentials) == 0 {
		return nil, errors.New("Response didn't contain credentials")
	}
	var blob s3Blob
	for _, c := range resp.Credentials {
		if err := json.Unmarshal([]byte(c.Blob), &blob); err != nil {
			return nil, err
		}
		if blob.Access == key {
			break
		}
	}

	return &blob, nil
}

// serverAuth return the X-Auth-Token to use or an error.
func (at *authToken) serverAuth(ctx context.Context, proxyCtx *ProxyContext, fresh bool) (string, error) {
	cacheKey := "Keystone:ServerAuth"
	var cachedServerAuth struct{ XSubjectToken string }
	if !fresh {
		if err := proxyCtx.Cache.GetStructured(ctx, cacheKey, &cachedServerAuth); err == nil {
			if cachedServerAuth.XSubjectToken != "" {
				return cachedServerAuth.XSubjectToken, nil
			}
		}
	}
	authReq := &identityReq{}
	authReq.Auth.Identity.Methods = []string{at.authPlugin}
	authReq.Auth.Identity.Password.User.Domain.ID = at.userDomainID
	authReq.Auth.Identity.Password.User.Name = at.userName
	authReq.Auth.Identity.Password.User.Password = at.password
	authReq.Auth.Scope.Project = &project{Domain: &domain{ID: at.projectDomainID}, Name: at.projectName}
	authReqBody, err := json.Marshal(authReq)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest("POST", at.authURL+"v3/auth/tokens", bytes.NewBuffer(authReqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(ctx)
	resp, err := at.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		return "", fmt.Errorf("server auth token request gave status %d", resp.StatusCode)
	}
	cachedServerAuth.XSubjectToken = resp.Header.Get("X-Subject-Token")
	proxyCtx.Cache.Set(ctx, cacheKey, cachedServerAuth, int(at.cacheDur/time.Second))
	return cachedServerAuth.XSubjectToken, nil
}

func removeAuthHeaders(r *http.Request) {
	for _, header := range authHeaders {
		r.Header.Del(header)
	}
}

func NewAuthToken(section conf.Section, metricsScope tally.Scope) (func(http.Handler) http.Handler, error) {
	return func(next http.Handler) http.Handler {
		tokenCacheDur := time.Duration(int(section.GetInt("token_cache_time", 300))) * time.Second
		c := &http.Client{
			Timeout: 5 * time.Second,
		}
		authTokenMiddleware := &authToken{
			next:           next,
			cacheDur:       tokenCacheDur,
			preValidateDur: (tokenCacheDur / 10),
			preValidations: make(map[string]bool),
			identity: &identity{authURL: section.GetDefault("auth_uri", "http://127.0.0.1:5000/"),
				authPlugin:      section.GetDefault("auth_plugin", "password"),
				projectDomainID: section.GetDefault("project_domain_id", "default"),
				userDomainID:    section.GetDefault("user_domain_id", "default"),
				projectName:     section.GetDefault("project_name", "service"),
				userName:        section.GetDefault("username", "swift"),
				password:        section.GetDefault("password", "password"),
				userAgent:       section.GetDefault("user_agent", "hummingbird-keystone-middleware/1.0"),
				client:          c},
		}
		if section.GetConfig().HasSection("tracing") {
			clientTracer, _, err := tracing.Init("proxy-keystone-client", zap.NewNop(), section.GetConfig().GetSection("tracing"))
			if err == nil {
				enableHTTPTrace := section.GetConfig().GetBool("tracing", "enable_httptrace", true)
				authTokenMiddleware.client, err = client.NewTracingClient(clientTracer, c, enableHTTPTrace)
				if err != nil { // In case of error revert to normal http client
					authTokenMiddleware.client = c
				}
			}
		}
		return authTokenMiddleware
	}, nil
}
