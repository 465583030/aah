// Copyright (c) Jeevanandam M. (https://github.com/jeevatkm)
// go-aah/aah source code and usage is governed by a MIT style
// license that can be found in the LICENSE file.

package aah

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"os"

	"aahframework.org/ahttp.v0"
	"aahframework.org/aruntime.v0"
	"aahframework.org/config.v0"
	"aahframework.org/essentials.v0"
	"aahframework.org/log.v0"
	"aahframework.org/pool.v0"
)

const (
	continuePipeline routeStatus = iota
	notContinuePipeline
)

const (
	aahServerName       = "aah-go-server"
	gzipContentEncoding = "gzip"
	hstsValue           = "max-age=31536000; includeSubDomains"
)

var errFileNotFound = errors.New("file not found")

type (
	// routeStatus is feadback of handleRoute method
	routeStatus uint8

	// Engine is the aah framework application server handler for request and response.
	// Implements `http.Handler` interface.
	engine struct {
		isRequestIDEnabled bool
		requestIDHeader    string
		isGzipEnabled      bool
		gzipLevel          int
		ctxPool            *pool.Pool
		reqPool            *pool.Pool
		bufPool            *pool.Pool
	}

	byName []os.FileInfo
)

//‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾
// Engine methods
//___________________________________

// ServeHTTP method implementation of http.Handler interface.
func (e *engine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if e.isRequestIDEnabled {
		e.setRequestID(r)
	}

	ctx := e.prepareContext(w, r)
	defer e.putContext(ctx)

	// Recovery handling, capture every possible panic(s)
	defer e.handleRecovery(ctx)

	// 'OnRequest' server extension point
	publishOnRequestEvent(ctx)

	// Handling route
	if e.handleRoute(ctx) == notContinuePipeline {
		return
	}

	// Load session
	e.loadSession(ctx)

	// Parsing request params
	e.parseRequestParams(ctx)

	// Set defaults when actual value not found
	e.setDefaults(ctx)

	// Middlewares, interceptors, targeted controller
	e.executeMiddlewares(ctx)

	// Write Reply on the wire
	e.writeReply(ctx)
}

// handleRecovery method handles application panics and recovers from it.
// Panic gets translated into HTTP Internal Server Error (Status 500).
func (e *engine) handleRecovery(ctx *Context) {
	if r := recover(); r != nil {
		log.Errorf("Internal Server Error on %s", ctx.Req.Path)

		st := aruntime.NewStacktrace(r, AppConfig())
		buf := e.getBuffer()
		defer e.putBuffer(buf)

		st.Print(buf)
		log.Error(buf.String())

		if AppProfile() == "prod" {
			ctx.Reply().InternalServerError().Text("500 Internal Server Error")
		} else { // detailed error info
			// TODO design server error page with stack trace info
			ctx.Reply().InternalServerError().Text("500 Internal Server Error: %s", buf.String())
		}

		e.writeReply(ctx)
	}
}

// setRequestID method sets the unique request id in the request header.
// It won't set new request id header already present.
func (e *engine) setRequestID(r *http.Request) {
	if ess.IsStrEmpty(r.Header.Get(e.requestIDHeader)) {
		guid := ess.NewGUID()
		log.Debugf("Request ID: %v", guid)
		r.Header.Set(e.requestIDHeader, guid)
	} else {
		log.Debugf("Request already has ID: %v", r.Header.Get(e.requestIDHeader))
	}
}

// prepareContext method gets controller, request from pool, set the targeted
// controller, parses the request and returns the controller.
func (e *engine) prepareContext(w http.ResponseWriter, req *http.Request) *Context {
	ctx, r := e.getContext(), e.getRequest()

	ctx.Req = ahttp.ParseRequest(req, r)
	ctx.reply = NewReply()
	ctx.viewArgs = make(map[string]interface{}, 0)

	if ctx.Req.IsGzipAccepted && e.isGzipEnabled {
		ctx.Res = ahttp.WrapGzipResponseWriter(w, e.gzipLevel)
	} else {
		ctx.Res = ahttp.WrapResponseWriter(w)
	}

	return ctx
}

// handleRoute method handle route processing for the incoming request.
// It does-
//  - finding domain
//  - finding route
//  - handling static route
//  - handling redirect trailing slash
//  - auto options
//  - route not found
//  - if route found then it sets targeted controller into context
//  - adds the pathParams into context if present
//
// Returns status as-
//  - continuePipeline
//  - notContinuePipeline
func (e *engine) handleRoute(ctx *Context) routeStatus {
	domain := AppRouter().FindDomain(ctx.Req)
	if domain == nil {
		ctx.Reply().NotFound().Text("404 Not Found")
		e.writeReply(ctx)
		return notContinuePipeline
	}

	route, pathParams, rts := domain.Lookup(ctx.Req)
	if route == nil { // route not found
		if err := handleRtsOptionsMna(ctx, domain, rts); err == nil {
			e.writeReply(ctx)
			return notContinuePipeline
		}

		ctx.route = domain.NotFoundRoute
		handleRouteNotFound(ctx, domain, domain.NotFoundRoute)
		e.writeReply(ctx)
		return notContinuePipeline
	}

	ctx.route = route
	ctx.domain = domain

	// Path parameters
	if pathParams.Len() > 0 {
		ctx.Req.Params.Path = make(map[string]string, pathParams.Len())
		for _, v := range *pathParams {
			ctx.Req.Params.Path[v.Key] = v.Value
		}
	}

	// Serving static file
	if route.IsStatic {
		if err := e.serveStatic(ctx); err == errFileNotFound {
			handleRouteNotFound(ctx, domain, route)
			e.writeReply(ctx)
		}
		return notContinuePipeline
	}

	// No controller or action found for the route
	if err := ctx.setTarget(route); err == errTargetNotFound {
		handleRouteNotFound(ctx, domain, route)
		e.writeReply(ctx)
		return notContinuePipeline
	}

	return continuePipeline
}

// loadSession method loads session from request for `stateful` session.
func (e *engine) loadSession(ctx *Context) {
	if AppSessionManager().IsStateful() {
		ctx.session = AppSessionManager().GetSession(ctx.Req.Raw)
		if ctx.session != nil {
			ctx.AddViewArg(keySessionValues, ctx.session)
		}
	}
}

// setDefaults method sets default value based on aah app configuration
// when actual value is not found.
func (e *engine) setDefaults(ctx *Context) {
	if ctx.Req.Locale == nil {
		ctx.Req.Locale = ahttp.NewLocale(AppConfig().StringDefault("i18n.default", "en"))
	}
}

// executeMiddlewares method executes the configured middlewares.
func (e *engine) executeMiddlewares(ctx *Context) {
	mwChain[0].Next(ctx)
}

// writeReply method writes the response on the wire based on `Reply` instance.
func (e *engine) writeReply(ctx *Context) {
	reply := ctx.Reply()

	if reply.done { // Response already written on the wire, don't go forward.
		return
	}

	if reply.redirect { // handle redirects
		log.Debugf("Redirecting to '%s' with status '%d'", reply.redirectURL, reply.Code)
		http.Redirect(ctx.Res, ctx.Req.Raw, reply.redirectURL, reply.Code)
		return
	}

	// Pre reply stage is for HTML and content type header
	handlePreReplyStage(ctx)

	// 'OnPreReply' server extension point
	publishOnPreReplyEvent(ctx)

	buf := e.getBuffer()
	defer e.putBuffer(buf)

	// Render and detect the errors earlier, framework can write error info
	// without messing with response.
	// HTTP Body
	if reply.Rdr != nil {
		if err := reply.Rdr.Render(buf); err != nil {
			log.Error("Render error: ", err)
			ctx.Res.WriteHeader(http.StatusInternalServerError)
			_, _ = ctx.Res.Write([]byte("500 Internal Server Error" + "\n"))
			return
		}
	}

	// Gzip
	e.writeGzipHeaders(ctx, buf.Len() == 0)

	// HTTP headers
	e.writeHeaders(ctx)

	// Set Cookies
	e.setCookies(ctx)

	// HTTP status
	ctx.Res.WriteHeader(reply.Code)

	// Write response buffer on the wire
	_, _ = buf.WriteTo(ctx.Res)

	// 'OnAfterReply' server extension point
	publishOnAfterReplyEvent(ctx)
}

// writeHeaders method writes the headers on the wire.
func (e *engine) writeHeaders(ctx *Context) {
	for k, v := range ctx.Reply().Hdr {
		for _, vv := range v {
			ctx.Res.Header().Add(k, vv)
		}
	}

	// ContentType, if it's not set then auto detect later in the writer
	if ctx.Reply().IsContentTypeSet() {
		ctx.Res.Header().Set(ahttp.HeaderContentType, ctx.Reply().ContType)
	}

	ctx.Res.Header().Set(ahttp.HeaderServer, aahServerName)

	// Set the HSTS if SSL is enabled on aah server
	// Know more: https://www.owasp.org/index.php/HTTP_Strict_Transport_Security_Cheat_Sheet
	if AppIsSSLEnabled() {
		ctx.Res.Header().Set(ahttp.HeaderStrictTransportSecurity, hstsValue)
	}
}

// writeGzipHeaders method prepares appropriate headers for gzip response
func (e *engine) writeGzipHeaders(ctx *Context, delCntEnc bool) {
	if ctx.Req.IsGzipAccepted && e.isGzipEnabled {
		ctx.Res.Header().Add(ahttp.HeaderVary, ahttp.HeaderAcceptEncoding)
		ctx.Res.Header().Add(ahttp.HeaderContentEncoding, gzipContentEncoding)
		ctx.Res.Header().Del(ahttp.HeaderContentLength)

		if ctx.Reply().Code == http.StatusNoContent || delCntEnc {
			ctx.Res.Header().Del(ahttp.HeaderContentEncoding)
		}
	}
}

// setCookies method sets the user cookies, session cookie and saves session
// into session store is session mode is stateful.
func (e *engine) setCookies(ctx *Context) {
	for _, c := range ctx.Reply().cookies {
		http.SetCookie(ctx.Res, c)
	}

	if AppSessionManager().IsStateful() && ctx.session != nil {
		if err := AppSessionManager().SaveSession(ctx.Res, ctx.session); err != nil {
			log.Error(err)
		}
	}
}

// getContext method gets context from pool
func (e *engine) getContext() *Context {
	return e.ctxPool.Get().(*Context)
}

// getRequest method gets request from pool
func (e *engine) getRequest() *ahttp.Request {
	return e.reqPool.Get().(*ahttp.Request)
}

// putContext method puts context back to pool
func (e *engine) putContext(ctx *Context) {
	// Try to close if `io.Closer` interface satisfies.
	ess.CloseQuietly(ctx.Res)

	// clear and put `ahttp.Request` into pool
	if ctx.Req != nil {
		ctx.Req.Reset()
		e.reqPool.Put(ctx.Req)
	}

	// clear and put `aah.Context` into pool
	ctx.Reset()
	e.ctxPool.Put(ctx)
}

// getBuffer method gets buffer from pool
func (e *engine) getBuffer() *bytes.Buffer {
	return e.bufPool.Get().(*bytes.Buffer)
}

// putBPool puts buffer into pool
func (e *engine) putBuffer(b *bytes.Buffer) {
	b.Reset()
	e.bufPool.Put(b)
}

//‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾
// Unexported methods
//___________________________________

func newEngine(cfg *config.Config) *engine {
	gzipLevel := cfg.IntDefault("render.gzip.level", 1)
	if !(gzipLevel >= 1 && gzipLevel <= 9) {
		logAsFatal(fmt.Errorf("'render.gzip.level' is not a valid level value: %v", gzipLevel))
	}

	return &engine{
		isRequestIDEnabled: cfg.BoolDefault("request.id.enable", true),
		requestIDHeader:    cfg.StringDefault("request.id.header", ahttp.HeaderXRequestID),
		isGzipEnabled:      cfg.BoolDefault("render.gzip.enable", true),
		gzipLevel:          gzipLevel,
		ctxPool: pool.NewPool(
			cfg.IntDefault("runtime.pooling.global", 0),
			func() interface{} {
				return &Context{}
			},
		),
		reqPool: pool.NewPool(
			cfg.IntDefault("runtime.pooling.global", 0),
			func() interface{} {
				return &ahttp.Request{}
			},
		),
		bufPool: pool.NewPool(
			cfg.IntDefault("runtime.pooling.buffer", 0),
			func() interface{} {
				return &bytes.Buffer{}
			},
		),
	}
}
