package handlers

import "net/http"

// SecurityHeadersMiddleware sets a conservative set of security headers. The
// CSP allows inline styles/scripts (Datastar uses inline attributes) and
// same-origin connections for SSE.
func SecurityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src 'self' 'unsafe-inline' 'unsafe-eval'; "+
				"style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data: blob:; "+
				"font-src 'self'; "+
				"connect-src 'self'; "+
				"media-src 'self' blob:")
		next.ServeHTTP(w, r)
	})
}
