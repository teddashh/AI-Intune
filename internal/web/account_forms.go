package web

import "net/http"

// RenderAccountForm deliberately does not require authenticated page data.
func (s *Server) RenderAccountForm(w http.ResponseWriter, setup bool, next, message string) {
	name := "login.html"
	if setup {
		name = "setup.html"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tmpl.ExecuteTemplate(w, name, struct{ Next, Error string }{next, message})
}
