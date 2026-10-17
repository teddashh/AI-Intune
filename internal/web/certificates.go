package web

import (
	"github.com/teddashh/AI-Intune/internal/certificates"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"net/http"
	"strconv"
	"strings"
)

type certificatesPage struct {
	Items  []certificates.Status
	Edit   certificates.Monitor
	Manage bool
}

func (s *Server) certificateRoutes(mux *http.ServeMux) []string {
	routes := []string{"GET /tenant/certificates", "POST /tenant/certificates"}
	for _, route := range routes {
		mux.HandleFunc(route, s.certificates)
	}
	return routes
}
func (s *Server) certificates(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	p, ok := operatorauth.PrincipalFromContext(r.Context())
	manage := ok && p.Has(operatorauth.Admin)
	if !ok || !p.Has(operatorauth.View) || r.Method == "POST" && !manage {
		http.Error(w, "Certificate access denied", 403)
		return
	}
	if r.Method == "POST" {
		if !operatorWebForm(w, r, nil, "operation", "id", "revision", "endpoint", "expected", "owner", "notes", "days") {
			http.Error(w, "Invalid certificate form", 400)
			return
		}
		revision, _ := strconv.ParseInt(r.PostForm.Get("revision"), 10, 64)
		id := r.PostForm.Get("id")
		actor := operator.ActorFromRequest(r, operator.SourceKindWeb)
		var err error
		switch r.PostForm.Get("operation") {
		case "delete":
			err = s.operator.CertificateDelete(id, revision, actor)
		case "probe":
			err = s.operator.CertificateProbe(id, revision, actor)
		case "save":
			c := certificates.Config{Endpoint: strings.TrimSpace(r.PostForm.Get("endpoint")), Expected: strings.Fields(strings.ReplaceAll(r.PostForm.Get("expected"), ",", " ")), Owner: r.PostForm.Get("owner"), Notes: r.PostForm.Get("notes")}
			for _, v := range strings.Fields(strings.ReplaceAll(r.PostForm.Get("days"), ",", " ")) {
				n, e := strconv.Atoi(v)
				if e != nil {
					http.Error(w, "Expiry days must be integers", 400)
					return
				}
				c.Days = append(c.Days, n)
			}
			_, err = s.operator.CertificateSave(id, revision, c, actor)
		default:
			http.Error(w, "Unknown certificate action", 400)
			return
		}
		if err != nil {
			s.renderActionStatus(w, r, 400, "Certificates", "Unable to save certificate monitor", "Check the fields or reload the page if another operator changed this monitor.", "/tenant/certificates")
			return
		}
		http.Redirect(w, r, "/tenant/certificates", http.StatusSeeOther)
		return
	}
	items, err := s.operator.CertificateRead()
	if err != nil {
		s.fail(w, "Unable to load certificates", err)
		return
	}
	v := &certificatesPage{Manage: manage, Items: []certificates.Status{}}
	v.Edit.Days = []int{30, 14, 7}
	for _, m := range items {
		v.Items = append(v.Items, m)
		if m.ID == r.URL.Query().Get("edit") {
			v.Edit = m.Monitor
		}
	}
	certificates.Sort(v.Items)
	s.render(w, r, "certificates.html", page{Title: "Certificates", Nav: "tenant-certificates", Certificates: v})
}
