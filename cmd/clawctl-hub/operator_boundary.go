package main

import (
	"fmt"
	"html"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/operatorendpoint"
	"github.com/teddashh/AI-Intune/internal/store"
	"github.com/teddashh/AI-Intune/internal/web"
)

// operatorRequestAuthorizer is deliberately narrower than the Tailscale
// client. Tests can exercise the HTTP boundary without talking to the host's
// tailscaled socket; the adapter itself is covered in internal/operatorauth.
type operatorRequestAuthorizer interface {
	Authorize(*http.Request, operatorauth.Permission) (*http.Request, operatorauth.Decision)
}

type operatorRepresentation uint8

const (
	operatorHTML operatorRepresentation = iota + 1
	operatorJSON
	// operatorPlain is the denial body for GET /metrics. A scraper should see
	// text, not an HTML page and not an operator JSON error.
	operatorPlain
)

type operatorRoutePolicy struct {
	Permission     operatorauth.Permission
	Representation operatorRepresentation
	SourceKind     string
	// SecurityProfile chooses the Content-Security-Policy. Representation
	// stays the denial body only; folding the profile into it would make one
	// field choose the error page, the script policy, and any later upgrade.
	// The zero value is not a profile, so a new route cannot gain one by
	// leaving the field out.
	SecurityProfile operatorSecurityProfile
}

type operatorSecurityProfile uint8

const (
	// operatorSecurityLocked is the policy every operator response used
	// before the terminal document existed: no script and no connection.
	operatorSecurityLocked operatorSecurityProfile = iota + 1
)

const (
	// The one unsafe route a view principal may call. It sets only the
	// caller's own navigation-language cookie, and the cross-origin check
	// still refuses a cross-site POST before the handler.
	navigationLanguagePattern = "POST /preferences/navigation-language"
)

func operatorSecurityProfileValid(_ string, profile operatorSecurityProfile) bool {
	return profile == operatorSecurityLocked
}

func operatorRepresentationValid(pattern string, representation operatorRepresentation) bool {
	switch representation {
	case operatorHTML, operatorJSON:
		return true
	case operatorPlain:
		return pattern == "GET /metrics"
	default:
		return false
	}
}

type nonOperatorRouteClass uint8

const (
	nonOperatorAgent nonOperatorRouteClass = iota + 1
	nonOperatorHealth
	nonOperatorMetrics
)

type nonOperatorRoutePolicy struct {
	Class nonOperatorRouteClass
}

// nonOperatorRoutePolicies is the other half of the routing security boundary.
// Routes on the root mux bypass human/operator authorization by design, so
// every one must be named here and classified as machine, liveness, or
// telemetry traffic. In particular, /v1/operator/* can never be classified as
// a machine route.
var nonOperatorRoutePolicies = map[string]nonOperatorRoutePolicy{
	"POST /v1/enrollments":             {nonOperatorAgent},
	"POST /v1/checkins":                {nonOperatorAgent},
	"POST /v1/observations:batch":      {nonOperatorAgent},
	"GET /v1/jobs/next":                {nonOperatorAgent},
	"GET /v1/agent/readiness":          {nonOperatorAgent},
	"POST /v1/jobs/{id}/claims":        {nonOperatorAgent},
	"POST /v1/jobs/{id}/lease:renew":   {nonOperatorAgent},
	"POST /v1/jobs/{id}/events":        {nonOperatorAgent},
	"POST /v1/jobs/{id}/verifications": {nonOperatorAgent},
	"POST /v1/verifications":           {nonOperatorAgent},
	"GET /v1/verification-assignments": {nonOperatorAgent},
	"POST /v1/jobs/{id}/complete":      {nonOperatorAgent},
	"POST /v1/jobs/{id}/reject":        {nonOperatorAgent},
	"GET /v1/capabilities":             {nonOperatorAgent},
	"GET /v1/artifacts/{sha256}":       {nonOperatorAgent},
	"HEAD /v1/artifacts/{sha256}":      {nonOperatorAgent},
	"GET /healthz":                     {nonOperatorHealth},
}

// operatorRoutePolicies is an explicit manifest, not a path-prefix rule.
// A newly registered control route therefore fails closed until somebody
// classifies that exact ServeMux pattern in review.
var operatorRoutePolicies = map[string]operatorRoutePolicy{
	"GET /metrics":                                                        {operatorauth.View, operatorPlain, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /{$}":                                                            {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /preferences/navigation-language":                               {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /machines":                                                       {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /machines/enrollment":                                            {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /machines/enrollment/limit-preview":                             {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /machines/enrollment/limits":                                    {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /downloads/agent/{arch}":                                         {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /machines/lifecycle":                                             {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /machines/configuration":                                         {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /machines/compliance":                                            {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /machines/diagnostics":                                           {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /machines/{id}":                                                  {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /jobs":                                                           {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /jobs/{id}":                                                      {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /apps":                                                           {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /apps/artifacts/{id}":                                            {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /apps/artifact-fetches/{id}":                                     {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /deployments":                                                    {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /deployments/{id}":                                               {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /updates":                                                        {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /reports/tickets":                                                {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /reports/tickets.csv":                                            {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /reports/changes":                                                {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /reports":                                                        {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /machines/{id}/timeline":                                         {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /machines/{id}/timeline.csv":                                     {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /machines/{id}/data":                                             {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /machines/{id}/data.csv":                                         {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /tenant/data":                                                    {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /reports/enrollment":                                             {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /reports/enrollment.csv":                                         {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /reports/software":                                               {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /reports/software.csv":                                           {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /reports/install":                                                {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /reports/install.csv":                                            {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /reports/profile":                                                {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /reports/profile.csv":                                            {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /audit":                                                          {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /settings/tailnet":                                               {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /tenant/maintenance":                                             {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /v1/operator/machines":                                           {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/machines/{id}":                                      {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/machines/{id}/evidence":                             {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/jobs":                                               {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/jobs/{id}":                                          {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/jobs/{id}/evidence":                                 {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/deployments":                                        {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/deployments/{id}":                                   {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/artifacts":                                          {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/artifacts/{sha256}":                                 {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/artifact-fetches/preview":                          {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/artifact-fetches":                                  {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/artifact-fetches":                                   {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/artifact-fetches/{id}":                              {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/updates":                                            {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/audit-events":                                       {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/changes":                                            {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/tickets":                                            {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/machines/{id}/lifecycle":                            {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/machines/{id}/lifecycle-preview":                   {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"PUT /v1/operator/machines/{id}/lifecycle":                            {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/machines/{id}/display-name-preview":                {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"PUT /v1/operator/machines/{id}/display-name":                         {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/machines/{id}/notes-preview":                       {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"PUT /v1/operator/machines/{id}/notes":                                {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/deployments/preview":                               {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/deployments":                                       {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/deployments/{id}/continuation-preview":             {operatorauth.Operate, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/deployments/{id}/continuations":                    {operatorauth.Operate, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/deployments/{id}/retry-preview":                    {operatorauth.Operate, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/deployments/{id}/retries":                          {operatorauth.Operate, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/deployments/{id}/abandonment-preview":              {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/deployments/{id}/abandonments":                     {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/machines/{id}/channel":                              {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/machines/{id}/assigned-user":                        {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/machines/{id}/enrollment-token":                     {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/machines/{id}/diagnostic-noop-preview":             {operatorauth.Operate, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/machines/{id}/diagnostic-noop-jobs":                {operatorauth.Operate, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/catalog-manifests":                                  {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/catalog-manifests":                                 {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/machine-profiles":                                   {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/machine-profiles":                                  {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/machines/{id}/profile-assignment-preview":          {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/machines/{id}/profile-assignments":                 {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/catalog-manifests/standard-preview":                {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/machine-profiles/preview":                          {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/tailnet":                                            {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/tailnet/peer-ignore-preview":                       {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"PUT /v1/operator/tailnet/peer-ignores/{id}":                          {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/maintenance/retention":                              {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/maintenance/retention/prune-preview":               {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/maintenance/retention/prunes":                      {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/maintenance/restore-drill-preview":                 {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/maintenance/restore-drills":                        {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/maintenance/restore-drills":                         {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/maintenance/restore-drills/{id}":                    {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/verifiers/preview":                                 {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/verifiers":                                         {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/verifiers":                                          {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/verifiers/{id}":                                     {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/verifiers/{id}/revocation-preview":                 {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/verifiers/{id}/revocations":                        {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/verifiers/{id}/assignment-preview":                 {operatorauth.Operate, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/verifiers/{id}/assignments":                        {operatorauth.Operate, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/settings":                                           {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/setting-policies/preview":                          {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/setting-policies":                                  {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/setting-assignments/preview":                       {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/setting-assignments":                               {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/compliance":                                         {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/compliance-policies/preview":                       {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/compliance-policies":                               {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/compliance-assignments/preview":                    {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/compliance-assignments":                            {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/machines/{id}/actions":                              {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/reports":                                            {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/daily-report":                                       {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/machines/{id}/timeline":                             {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/data-disclosure":                                    {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/enrollment-report":                                  {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/software-report":                                    {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/install-report":                                     {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/profile-report":                                     {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/enrollment-limit":                                   {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/enrollment-limit/preview":                          {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/enrollment-limit":                                  {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/machines/{id}/data":                                 {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /machines/{id}/connect":                                         {operatorauth.Operate, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /apps/artifact-fetches/preview":                                 {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /apps/artifact-fetches":                                         {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /apps/store/packages/preview":                                   {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /apps/store/packages":                                           {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /machines/configuration/policy-preview":                         {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /machines/configuration/policies":                               {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /machines/configuration/assignment-preview":                     {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /machines/configuration/assignments":                            {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /machines/compliance/policy-preview":                            {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /machines/compliance/policies":                                  {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /machines/compliance/assignment-preview":                        {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /machines/compliance/assignments":                               {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /apps/profiles/preview":                                         {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /apps/profiles":                                                 {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /apps/profile-assignments/preview":                              {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /apps/profile-assignments":                                      {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /deployments/preview":                                           {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /deployments":                                                   {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /deployments/{id}/continue-preview":                             {operatorauth.Operate, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /deployments/{id}/continue":                                     {operatorauth.Operate, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /deployments/{id}/retry-preview":                                {operatorauth.Operate, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /deployments/{id}/retry":                                        {operatorauth.Operate, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /deployments/{id}/abandon-preview":                              {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /deployments/{id}/abandon":                                      {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /machines/{id}/retire":                                          {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /machines/{id}/unretire":                                        {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /machines/{id}/lifecycle-preview":                               {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /machines/{id}/diagnostic-noop-preview":                         {operatorauth.Operate, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /machines/{id}/diagnostic-noop-jobs":                            {operatorauth.Operate, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /enrollments":                                                   {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /enrollments/preview":                                           {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /machines/{id}/revoke-token/preview":                            {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /machines/{id}/revoke-token":                                    {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /machines/{id}/channel":                                         {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /machines/{id}/assigned-user":                                   {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /machines/{id}/assigned-user-preview":                           {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /machines/{id}/display-name-preview":                            {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /machines/{id}/display-name":                                    {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /machines/{id}/notes-preview":                                   {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /machines/{id}/notes":                                           {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /jobs/{id}/verifier-assignment-preview":                         {operatorauth.Operate, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /jobs/{id}/verifier-assignments":                                {operatorauth.Operate, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /settings/tailnet/peer-ignore-preview":                          {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /settings/tailnet/peer-ignores":                                 {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /tenant/maintenance/retention/prune-preview":                    {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /tenant/maintenance/retention/prunes":                           {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"GET /tenant/maintenance/restore-drills/{id}":                         {operatorauth.View, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /tenant/maintenance/restore-drill-preview":                      {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /tenant/maintenance/restore-drills":                             {operatorauth.Admin, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"PUT /v1/operator/machines/{id}/channel":                              {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"PUT /v1/operator/machines/{id}/assigned-user":                        {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/enrollment-tokens/preview":                         {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/enrollment-tokens":                                 {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/machines/{id}/enrollment-token/revocation-preview": {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/machines/{id}/enrollment-token/revocations":        {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /deployments/{id}/skip-failed-batch-preview":                    {operatorauth.Operate, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /deployments/{id}/skip-failed-batch":                            {operatorauth.Operate, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked},
	"POST /v1/operator/deployments/{id}/skip-failed-batch-preview":        {operatorauth.Operate, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/deployments/{id}/skip-failed-batches":              {operatorauth.Operate, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/disk-clean/summaries":                               {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"GET /v1/operator/disk-clean/summaries/{id}":                          {operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/disk-clean/profile-preview":                        {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/disk-clean/profiles":                               {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/disk-clean/dry-run-preview":                        {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/disk-clean/dry-runs":                               {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/disk-clean/canary-preview":                         {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/disk-clean/canaries":                               {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/disk-clean/continuation-preview":                   {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/disk-clean/continuations":                          {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/disk-clean/abandonment-preview":                    {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
	"POST /v1/operator/disk-clean/abandonments":                           {operatorauth.Admin, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked},
}

const (
	csrfDecisionCode                 = "CROSS_ORIGIN_REQUEST"
	operatorAuthorityDecisionCode    = "OPERATOR_AUTHORITY_REQUIRED"
	denialRateLimitedDecisionCode    = "OPERATOR_DENIALS_RATE_LIMITED"
	denialAuditTruncatedDecisionCode = "OPERATOR_DENIAL_AUDIT_TRUNCATED"
	denialLogBurst                   = 12
	denialAuditBurst                 = 12
	denialAuditWindow                = time.Minute
)

type operatorBoundary struct {
	next       *http.ServeMux
	authorizer operatorRequestAuthorizer
	store      *store.Store
	policies   map[string]operatorRoutePolicy
	authority  string
	csrfNext   http.Handler
	denials    *operatorDenialLimiter
}

func newOperatorBoundary(next *http.ServeMux, authorizer operatorRequestAuthorizer,
	st *store.Store, policies map[string]operatorRoutePolicy, authority string,
) *operatorBoundary {
	canonicalAuthority, _ := canonicalLiteralAuthority(authority)
	b := &operatorBoundary{
		next: next, authorizer: authorizer, store: st, policies: policies,
		authority: canonicalAuthority, denials: newOperatorDenialLimiter(time.Now),
	}
	csrf := http.NewCrossOriginProtection()
	csrf.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pattern := next.Handler(r)
		policy := policies[pattern]
		b.observeBoundaryDenial(r, pattern, policy, csrfDecisionCode,
			"cross_origin_mutation")
		writeOperatorBoundaryError(w, policy.Representation, http.StatusForbidden, csrfDecisionCode,
			"跨來源寫入已拒絕")
	}))
	b.csrfNext = csrf.Handler(next)
	return b
}

// newHubHTTPHandler keeps the machine plane outside the human/operator auth
// middleware. A machine bearer token can never acquire operator authority, and
// a LocalAPI outage can never stop agents from checking in.
func newHubHTTPHandler(h *hub, ui *web.Server, authorizer operatorRequestAuthorizer,
	authority string,
) (http.Handler, error) {
	if canonical, ok := canonicalLiteralAuthority(authority); !ok || canonical != authority {
		return nil, fmt.Errorf("operator authority %q 必須是 canonical literal-ip:port", authority)
	}
	root := http.NewServeMux()
	nonOperatorRegistered := h.machineAndPublicRoutes(root)

	operatorMux := http.NewServeMux()
	operatorRegistered := h.operatorRoutes(operatorMux)
	operatorRegistered = append(operatorRegistered, ui.Routes(operatorMux)...)
	if err := validateRouteManifests(nonOperatorRegistered, nonOperatorRoutePolicies,
		operatorRegistered, operatorRoutePolicies); err != nil {
		return nil, err
	}
	root.Handle("/", newOperatorBoundary(operatorMux, authorizer, h.store, operatorRoutePolicies, authority))
	return root, nil
}

func validateRouteManifests(nonOperatorRegistered []string,
	nonOperatorPolicies map[string]nonOperatorRoutePolicy,
	operatorRegistered []string, operatorPolicies map[string]operatorRoutePolicy,
) error {
	if err := validateNonOperatorRoutePolicies(nonOperatorRegistered, nonOperatorPolicies); err != nil {
		return err
	}
	if err := validateOperatorRoutePolicies(operatorRegistered, operatorPolicies); err != nil {
		return err
	}
	for pattern := range nonOperatorPolicies {
		if _, overlaps := operatorPolicies[pattern]; overlaps {
			return fmt.Errorf("route %q 同時出現在 operator 與 non-operator manifest", pattern)
		}
	}
	return nil
}

func validateNonOperatorRoutePolicies(registered []string, policies map[string]nonOperatorRoutePolicy) error {
	seen := make(map[string]struct{}, len(registered))
	for _, pattern := range registered {
		if _, duplicate := seen[pattern]; duplicate {
			return fmt.Errorf("non-operator route %q 註冊了兩次", pattern)
		}
		seen[pattern] = struct{}{}
		policy, ok := policies[pattern]
		if !ok {
			return fmt.Errorf("non-operator route %q 沒有 plane policy", pattern)
		}
		method, path, valid := strings.Cut(pattern, " ")
		if !valid || !validRouteMethod(method) || path == "" {
			return fmt.Errorf("non-operator route %q 不是 method/path pattern", pattern)
		}
		switch policy.Class {
		case nonOperatorAgent:
			if !strings.HasPrefix(path, "/v1/") {
				return fmt.Errorf("non-operator route %q 不是合法 machine-plane path", pattern)
			}
			// The second segment is the plane discriminator. Requiring it to
			// be literal prevents patterns such as /v1/{plane}/... or
			// /v1/{rest...} from also matching /v1/operator/* on the root mux.
			segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
			if len(segments) < 2 || segments[0] != "v1" ||
				!validMachinePlaneDiscriminator(segments[1]) {
				return fmt.Errorf("non-operator route %q 的 machine-plane discriminator 必須是 literal", pattern)
			}
		case nonOperatorHealth:
			if pattern != "GET /healthz" {
				return fmt.Errorf("non-operator health route %q 不合法", pattern)
			}
		case nonOperatorMetrics:
			// /metrics is an operator view route. Keeping the class lets tests
			// prove a public registration is rejected, including aliases.
			return fmt.Errorf("non-operator route %q 不得公開 /metrics；它要 operator view", pattern)
		default:
			return fmt.Errorf("non-operator route %q 的 plane policy 不合法", pattern)
		}
	}
	for pattern := range policies {
		if _, ok := seen[pattern]; !ok {
			return fmt.Errorf("non-operator plane policy %q 沒有對應 route", pattern)
		}
	}
	return nil
}

func validMachinePlaneDiscriminator(segment string) bool {
	if segment == "" || segment == "operator" {
		return false
	}
	for i := 0; i < len(segment); i++ {
		c := segment[i]
		if (c >= 'a' && c <= 'z') || (i > 0 && c >= '0' && c <= '9') ||
			(i > 0 && (c == '-' || c == ':')) {
			continue
		}
		return false
	}
	return true
}

func validRouteMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions,
		http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func validateOperatorRoutePolicies(registered []string, policies map[string]operatorRoutePolicy) error {
	seen := make(map[string]struct{}, len(registered))
	for _, pattern := range registered {
		if _, duplicate := seen[pattern]; duplicate {
			return fmt.Errorf("operator route %q 註冊了兩次", pattern)
		}
		seen[pattern] = struct{}{}
		policy, ok := policies[pattern]
		if !ok {
			return fmt.Errorf("operator route %q 沒有 capability policy", pattern)
		}
		if policy.Permission < operatorauth.View || policy.Permission > operatorauth.Admin ||
			!operatorRepresentationValid(pattern, policy.Representation) ||
			(policy.SourceKind != operator.SourceKindWeb && policy.SourceKind != operator.SourceKindOperatorAPI) ||
			!operatorSecurityProfileValid(pattern, policy.SecurityProfile) {
			return fmt.Errorf("operator route %q 的 policy 不合法", pattern)
		}
		method, path, valid := strings.Cut(pattern, " ")
		if !valid || !validRouteMethod(method) || path == "" {
			return fmt.Errorf("operator route %q 不是受支援的 method/path pattern", pattern)
		}
		if !isSafeMethod(method) && policy.Permission == operatorauth.View && pattern != navigationLanguagePattern {
			return fmt.Errorf("unsafe operator route %q 不得分類為 view", pattern)
		}
		if policy.SourceKind == operator.SourceKindOperatorAPI {
			if !strings.HasPrefix(path, "/v1/operator/") {
				return fmt.Errorf("operator API route %q 必須在 /v1/operator/ namespace", pattern)
			}
		}
	}
	for pattern := range policies {
		if _, ok := seen[pattern]; !ok {
			return fmt.Errorf("operator capability policy %q 沒有對應 route", pattern)
		}
	}
	return nil
}

func (b *operatorBoundary) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, pattern := b.next.Handler(r)
	if pattern == "" {
		// No registered control route matched. Preserve ServeMux's canonical
		// 404/405 response; there is no handler here that could mutate state.
		// An unknown path stays on the locked policy.
		b.writeSecurityHeaders(w, r, operatorSecurityLocked)
		b.next.ServeHTTP(w, r)
		return
	}
	policy, ok := b.policies[pattern]
	if !ok || !operatorPolicyAdmissible(pattern, policy) {
		b.writeSecurityHeaders(w, r, operatorSecurityLocked)
		b.observeBoundaryDenial(r, pattern, operatorRoutePolicy{}, string(operatorauth.AuthConfigurationInvalid),
			"operator_route_policy_invalid")
		writeOperatorBoundaryError(w, operatorHTML, http.StatusServiceUnavailable,
			string(operatorauth.AuthConfigurationInvalid), "控制面路由不可用")
		return
	}
	b.writeSecurityHeaders(w, r, policy.SecurityProfile)
	// CrossOriginProtection validates browser provenance, but a browser can call
	// a hostile hostname that DNS-rebinds to the Hub and still truthfully send
	// Sec-Fetch-Site: same-origin. Pinning Host to the configured literal
	// listener authority closes that gap; Host is routing input, never identity.
	if requestAuthority, valid := canonicalLiteralAuthority(r.Host); !valid || requestAuthority != b.authority {
		b.observeBoundaryDenial(r, pattern, policy, operatorAuthorityDecisionCode,
			"HTTP Host 與啟動時釘住的 operator authority 不符")
		writeOperatorBoundaryError(w, policy.Representation, http.StatusMisdirectedRequest,
			operatorAuthorityDecisionCode, "請使用 Hub 明示的 Tailscale IP 與 port")
		return
	}
	if b.authorizer == nil {
		b.observeAuthDenial(r, pattern, policy, string(operatorauth.AuthConfigurationInvalid),
			"operator_auth_unavailable", operatorauth.Principal{}, nil)
		writeOperatorBoundaryError(w, policy.Representation, http.StatusServiceUnavailable,
			string(operatorauth.AuthConfigurationInvalid), "控制面身分驗證不可用")
		return
	}

	before := routingOf(r)
	authed, decision := b.authorizer.Authorize(r, policy.Permission)
	if !decision.Allowed || authed == nil {
		cause := decision.Cause()
		if decision.Allowed || authed != nil || !validAuthorizationDenial(decision) {
			cause = fmt.Errorf("operator auth adapter returned contradictory result: pattern=%q allowed=%t request_nil=%t status=%d code=%q",
				pattern, decision.Allowed, authed == nil, decision.HTTPStatus, decision.Code)
			decision = operatorauth.Decision{
				HTTPStatus: http.StatusServiceUnavailable, Code: operatorauth.AuthConfigurationInvalid,
				Detail: operatorAuthAdapterContradictoryDetail,
			}
		}
		b.observeAuthDenial(r, pattern, policy, string(decision.Code), decision.Detail,
			decision.Principal, cause)
		writeOperatorBoundaryError(w, policy.Representation, decision.HTTPStatus, string(decision.Code), decision.Detail)
		return
	}
	_, breach := validateAuthorizedResult(before, authed, decision, policy.Permission)
	if breach != "" {
		b.observeAuthDenial(r, pattern, policy, string(operatorauth.AuthConfigurationInvalid),
			string(breach), operatorauth.Principal{}, nil)
		writeOperatorBoundaryError(w, policy.Representation, http.StatusServiceUnavailable,
			string(operatorauth.AuthConfigurationInvalid), "控制面身分驗證回傳不完整")
		return
	}

	b.csrfNext.ServeHTTP(w, authed)
}

func validAuthorizationDenial(decision operatorauth.Decision) bool {
	if decision.Allowed {
		return false
	}
	switch decision.Code {
	case operatorauth.Unauthenticated:
		return decision.HTTPStatus == http.StatusUnauthorized
	case operatorauth.HumanPrincipalRequired, operatorauth.CapabilityRequired:
		return decision.HTTPStatus == http.StatusForbidden
	case operatorauth.AuthSourceUnavailable, operatorauth.AuthConfigurationInvalid:
		return decision.HTTPStatus == http.StatusServiceUnavailable
	default:
		return false
	}
}

// requestRouting 是呼叫 Authorize 之前那一刻的 routing 欄位取值快照。
// http.Request.WithContext 是 shallow copy，adapter 交回來的 request 與
// 原本那份共用同一個 *url.URL，就地改動兩邊會一起變；必須先取值才比得出來。
// principal 是 Tailscale LocalAPI 用 remoteAddr 的 peer 位址 WhoIs 出來的，
// 稽核也把這次動作歸給這個位址；adapter 事後改掉它，等於把判決綁到
// 另一個來源上。
type requestRouting struct {
	method, host, remoteAddr, requestURI             string
	scheme, urlHost, path, rawPath, rawQuery, opaque string
	forceQuery                                       bool
	complete                                         bool
}

// operatorAuthBreach 指名授權 adapter 的成功判決破了哪一條不變式。
// 每個值都是本檔擁有的字面值：稽核列會原樣帶走它，adapter 供應的任何字串
// 都碰不到這條路。空字串代表判決通過。
type operatorAuthBreach string

// adapter 宣稱放行卻沒交回 request、或拒絕判決的 code 與 status 對不上時，
// boundary 用自己的說明覆蓋 adapter 那一份：不能讓一份自相矛盾的判決
// 決定稽核列上寫什麼。
const operatorAuthAdapterContradictoryDetail = "operator auth adapter 回傳不一致的判決"

const (
	operatorAuthSuccessRequestIncomplete         operatorAuthBreach = "operator_auth_success_request_incomplete"
	operatorAuthSuccessPrincipalMissing          operatorAuthBreach = "operator_auth_success_principal_missing"
	operatorAuthSuccessDecisionNotAuthorized     operatorAuthBreach = "operator_auth_success_decision_not_authorized"
	operatorAuthSuccessDecisionPrincipalDiffers  operatorAuthBreach = "operator_auth_success_decision_principal_differs"
	operatorAuthSuccessIdentitySubjectEmpty      operatorAuthBreach = "operator_auth_success_identity_subject_empty"
	operatorAuthSuccessIdentityNodeEmpty         operatorAuthBreach = "operator_auth_success_identity_node_empty"
	operatorAuthSuccessIdentityLoginEmpty        operatorAuthBreach = "operator_auth_success_identity_login_empty"
	operatorAuthSuccessIdentityMethodNotLocalAPI operatorAuthBreach = "operator_auth_success_identity_method_not_localapi"
	operatorAuthSuccessPermissionUnclassified    operatorAuthBreach = "operator_auth_success_permission_unclassified"
	operatorAuthSuccessCapabilityNotGranted      operatorAuthBreach = "operator_auth_success_capability_not_granted"
	operatorAuthSuccessCapabilityDiffers         operatorAuthBreach = "operator_auth_success_capability_differs"
	operatorAuthSuccessRoutingChanged            operatorAuthBreach = "operator_auth_success_routing_changed"
	operatorAuthSuccessSourceNotPeer             operatorAuthBreach = "operator_auth_success_source_not_peer"
)

func routingOf(r *http.Request) requestRouting {
	if r == nil || r.URL == nil {
		return requestRouting{}
	}
	return requestRouting{
		method: r.Method, host: r.Host, remoteAddr: r.RemoteAddr, requestURI: r.RequestURI,
		scheme: r.URL.Scheme, urlHost: r.URL.Host, path: r.URL.Path, rawPath: r.URL.RawPath,
		rawQuery: r.URL.RawQuery, opaque: r.URL.Opaque, forceQuery: r.URL.ForceQuery,
		complete: true,
	}
}

func validateAuthorizedResult(before requestRouting, authed *http.Request, decision operatorauth.Decision,
	required operatorauth.Permission,
) (operatorauth.Principal, operatorAuthBreach) {
	after := routingOf(authed)
	if !after.complete {
		return operatorauth.Principal{}, operatorAuthSuccessRequestIncomplete
	}
	principal, ok := operatorauth.PrincipalFromContext(authed.Context())
	if !ok {
		return operatorauth.Principal{}, operatorAuthSuccessPrincipalMissing
	}
	if decision.Code != operatorauth.Authorized || decision.HTTPStatus != http.StatusOK {
		return principal, operatorAuthSuccessDecisionNotAuthorized
	}
	if decision.Principal != principal {
		return principal, operatorAuthSuccessDecisionPrincipalDiffers
	}
	if principal.StableSubject() == "" {
		return principal, operatorAuthSuccessIdentitySubjectEmpty
	}
	if principal.NodeStableID == "" {
		return principal, operatorAuthSuccessIdentityNodeEmpty
	}
	if principal.TailnetUserLogin == "" {
		return principal, operatorAuthSuccessIdentityLoginEmpty
	}
	if principal.AuthMethod != operatorauth.AuthMethodLocalAPI {
		return principal, operatorAuthSuccessIdentityMethodNotLocalAPI
	}
	var expectedCapability string
	switch required {
	case operatorauth.View:
		expectedCapability = principal.GrantedCapabilities.View
	case operatorauth.Operate:
		expectedCapability = principal.GrantedCapabilities.Operate
	case operatorauth.Admin:
		expectedCapability = principal.GrantedCapabilities.Admin
	default:
		return principal, operatorAuthSuccessPermissionUnclassified
	}
	if expectedCapability == "" {
		return principal, operatorAuthSuccessCapabilityNotGranted
	}
	if principal.AuthorizedCapability != expectedCapability {
		return principal, operatorAuthSuccessCapabilityDiffers
	}
	if before != after {
		return principal, operatorAuthSuccessRoutingChanged
	}
	if !principal.MatchesPeer(before.remoteAddr) {
		return principal, operatorAuthSuccessSourceNotPeer
	}
	return principal, ""
}

func canonicalLiteralAuthority(authority string) (string, bool) {
	return operatorendpoint.CanonicalLiteralAuthority(authority)
}

func operatorPolicyAdmissible(pattern string, policy operatorRoutePolicy) bool {
	if policy.Permission < operatorauth.View || policy.Permission > operatorauth.Admin {
		return false
	}
	return operatorSecurityProfileValid(pattern, policy.SecurityProfile)
}

// lockedOperatorContentSecurityPolicy is the policy every operator response
// still sends. It names no script and no connection.
const lockedOperatorContentSecurityPolicy = "default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

func (b *operatorBoundary) writeSecurityHeaders(w http.ResponseWriter, r *http.Request, profile operatorSecurityProfile) {
	header := w.Header()
	header.Set("Cache-Control", "no-store")
	header.Set("Content-Security-Policy", contentSecurityPolicy(profile, b.authority, r))
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "DENY")
}

func contentSecurityPolicy(operatorSecurityProfile, string, *http.Request) string {
	return lockedOperatorContentSecurityPolicy
}

func writeOperatorBoundaryError(w http.ResponseWriter, representation operatorRepresentation, status int, code, detail string) {
	switch representation {
	case operatorJSON:
		writeErr(w, status, code, detail)
		return
	case operatorPlain:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, "%s: %s\n", code, detail)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "<!doctype html><html lang=\"zh-Hant\"><meta charset=\"utf-8\"><title>clawctl 請求遭拒</title><body><main><h1>請求遭拒</h1><p>%s</p><p><code>%s</code></p></main></body></html>",
		html.EscapeString(detail), html.EscapeString(code))
}

type operatorDenialLimiter struct {
	mu                    sync.Mutex
	now                   func() time.Time
	windowStart           time.Time
	emittedLogs           int
	persistedUnsafe       int
	suppressedLogs        uint64
	suppressedUnsafeAudit uint64
	lastLogSource         string
	lastUnsafeSource      string
}

type operatorDenialAdmission struct {
	emitLog, persistUnsafe           bool
	auditTruncated                   bool
	truncatedAt                      time.Time
	windowStart, windowEnd           time.Time
	suppressedLogs, suppressedUnsafe uint64
	lastLogSource                    string
	lastUnsafeSource                 string
}

func newOperatorDenialLimiter(now func() time.Time) *operatorDenialLimiter {
	if now == nil {
		now = time.Now
	}
	return &operatorDenialLimiter{now: now}
}

// admit is global to the Hub process rather than keyed by caller-controlled
// path/source. Fixed cardinality matters here: otherwise an attacker can evade
// the limiter by rotating wildcard path values or source ports.
func (l *operatorDenialLimiter) admit(unsafe bool, source string) operatorDenialAdmission {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	result := operatorDenialAdmission{}
	if l.windowStart.IsZero() {
		l.windowStart = now
	} else if now.Before(l.windowStart) || now.Sub(l.windowStart) >= denialAuditWindow {
		result.windowStart, result.windowEnd = l.windowStart, now
		result.suppressedLogs = l.suppressedLogs
		result.suppressedUnsafe = l.suppressedUnsafeAudit
		result.lastLogSource = l.lastLogSource
		result.lastUnsafeSource = l.lastUnsafeSource
		l.windowStart, l.emittedLogs, l.persistedUnsafe = now, 0, 0
		l.suppressedLogs, l.suppressedUnsafeAudit = 0, 0
		l.lastLogSource, l.lastUnsafeSource = "", ""
	}
	if l.emittedLogs < denialLogBurst {
		l.emittedLogs++
		result.emitLog = true
	} else {
		l.suppressedLogs++
		l.lastLogSource = source
	}
	if unsafe {
		if l.persistedUnsafe < denialAuditBurst {
			l.persistedUnsafe++
			result.persistUnsafe = true
		} else {
			if l.suppressedUnsafeAudit == 0 {
				result.auditTruncated = true
				result.truncatedAt = now
			}
			l.suppressedUnsafeAudit++
			l.lastUnsafeSource = source
		}
	}
	return result
}

func (b *operatorBoundary) observeBoundaryDenial(r *http.Request, pattern string,
	policy operatorRoutePolicy, boundaryDecision, detail string,
) {
	principal, _ := operatorauth.PrincipalFromContext(r.Context())
	authDecision := ""
	if principal.StableSubject() != "" {
		authDecision = string(operatorauth.Authorized)
	}
	b.observeDenial(r, pattern, policy, authDecision, boundaryDecision, detail, principal, nil)
}

func (b *operatorBoundary) observeAuthDenial(r *http.Request, pattern string,
	policy operatorRoutePolicy, authDecision, detail string, principal operatorauth.Principal,
	cause error,
) {
	b.observeDenial(r, pattern, policy, authDecision, "", detail, principal, cause)
}

func (b *operatorBoundary) observeDenial(r *http.Request, pattern string,
	policy operatorRoutePolicy, authDecision, boundaryDecision, detail string,
	principal operatorauth.Principal, cause error,
) {
	unsafe := !isSafeMethod(r.Method)
	source := denialSource(r, principal)
	admission := b.denials.admit(unsafe, source)
	if admission.suppressedLogs != 0 || admission.suppressedUnsafe != 0 {
		log.Printf("⚠ operator denial rate limit：window=%s..%s suppressed_logs=%d suppressed_unsafe_audit=%d last_log_source=%q last_unsafe_source=%q",
			admission.windowStart.UTC().Format(time.RFC3339), admission.windowEnd.UTC().Format(time.RFC3339),
			admission.suppressedLogs, admission.suppressedUnsafe,
			boundedLogValue(admission.lastLogSource), boundedLogValue(admission.lastUnsafeSource))
		if admission.suppressedUnsafe != 0 {
			b.persistSuppressedDenials(admission)
		}
	}
	if admission.emitLog {
		log.Printf("operator request denied method=%q path=%q host=%q source=%q auth=%q boundary=%q detail=%q cause=%q audit_admitted=%t audit_store_available=%t",
			boundedLogValue(r.Method), boundedLogValue(requestPath(r)), boundedLogValue(r.Host),
			boundedLogValue(source), boundedLogValue(authDecision), boundedLogValue(boundaryDecision),
			boundedLogValue(detail), boundedLogValue(errorText(cause)), admission.persistUnsafe, b.store != nil)
	}
	if admission.persistUnsafe {
		b.persistDenial(r, pattern, policy, authDecision, boundaryDecision, detail, principal)
	}
	if admission.auditTruncated {
		b.persistDenialAuditTruncated(admission, source)
	}
}

func (b *operatorBoundary) persistDenial(r *http.Request, pattern string,
	policy operatorRoutePolicy, authDecision, boundaryDecision, detail string,
	principal operatorauth.Principal,
) {
	if b.store == nil {
		return
	}
	sourceAddr := principal.SourceAddr
	if sourceAddr == "" {
		sourceAddr = r.RemoteAddr
	}
	required := policy.Permission.String()
	if policy.Permission < operatorauth.View || policy.Permission > operatorauth.Admin {
		required = "unclassified"
	}
	sourceKind := policy.SourceKind
	if sourceKind == "" {
		sourceKind = "unclassified"
	}
	machineID, subject := b.denialSubject(r)
	decision := authDecision
	if decision == "" {
		decision = boundaryDecision
	}
	entry := store.AuditEntry{
		Action: store.AuditOperatorDenied, MachineID: machineID, Subject: subject,
		SourceAddr: sourceAddr, WhoNode: principal.DeviceName, WhoUser: principal.TailnetUserLogin,
		UserAgent: r.UserAgent(), AuthSubject: principal.StableSubject(),
		AuthNodeID: principal.NodeStableID, AuthCapability: principal.AuthorizedCapability,
		AuthMethod: principal.AuthMethod, AuthDecision: authDecision,
		BoundaryDecision: boundaryDecision, SourceKind: sourceKind,
		OK: false,
		Detail: store.OperatorBoundaryDenialPrefix + "code=" + decision + "；required=" + required +
			"；pattern=" + pattern + "；" + detail,
	}
	if err := b.store.RecordOperatorDenial(entry); err != nil {
		log.Printf("⚠ operator boundary denial audit 寫不進去 auth=%s boundary=%s pattern=%q: %v",
			authDecision, boundaryDecision, pattern, err)
	}
}

func (b *operatorBoundary) persistSuppressedDenials(admission operatorDenialAdmission) {
	if b.store == nil {
		return
	}
	entry := store.AuditEntry{
		At: admission.windowEnd, Action: store.AuditOperatorDenied,
		Subject: "operator denial rate limiter", SourceAddr: admission.lastUnsafeSource,
		BoundaryDecision: denialRateLimitedDecisionCode, SourceKind: "operator-boundary-summary",
		OK: false,
		Detail: fmt.Sprintf("%scode=%s；suppressed_logs=%d；suppressed_unsafe_audit=%d；window=%s..%s",
			store.OperatorBoundaryDenialPrefix, denialRateLimitedDecisionCode,
			admission.suppressedLogs, admission.suppressedUnsafe,
			admission.windowStart.UTC().Format(time.RFC3339), admission.windowEnd.UTC().Format(time.RFC3339)),
	}
	if err := b.store.RecordOperatorDenial(entry); err != nil {
		log.Printf("⚠ operator denial rate-limit summary 寫不進 audit：%v", err)
	}
}

func (b *operatorBoundary) persistDenialAuditTruncated(admission operatorDenialAdmission, source string) {
	if b.store == nil {
		return
	}
	entry := store.AuditEntry{
		At: admission.truncatedAt, Action: store.AuditOperatorDenied,
		Subject: "operator denial rate limiter", SourceAddr: source,
		BoundaryDecision: denialAuditTruncatedDecisionCode, SourceKind: "operator-boundary-summary",
		OK: false,
		Detail: store.OperatorBoundaryDenialPrefix + "code=" + denialAuditTruncatedDecisionCode +
			"；此視窗寫入型拒絕的逐筆稽核已達上限；後續同類拒絕不另列。",
	}
	if err := b.store.RecordOperatorDenial(entry); err != nil {
		log.Printf("⚠ operator denial audit-truncated marker 寫不進 audit：%v", err)
	}
}

func denialSource(r *http.Request, principal operatorauth.Principal) string {
	if principal.SourceAddr != "" {
		return principal.SourceAddr
	}
	if r == nil {
		return ""
	}
	if addrPort, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		return addrPort.Addr().Unmap().String()
	}
	if addr, err := netip.ParseAddr(r.RemoteAddr); err == nil {
		return addr.Unmap().String()
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func requestPath(r *http.Request) string {
	if r == nil || r.URL == nil {
		return ""
	}
	return r.URL.Path
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func boundedLogValue(value string) string {
	const maxRunes = 240
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return string(runes[:maxRunes]) + "…(truncated)"
}

func (b *operatorBoundary) denialSubject(r *http.Request) (machineID, subject string) {
	subject = r.Method + " " + r.URL.Path
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	candidate := ""
	switch {
	case len(parts) >= 2 && parts[0] == "machines":
		candidate = parts[1]
	case len(parts) >= 4 && parts[0] == "v1" && parts[1] == "operator" && parts[2] == "machines":
		candidate = parts[3]
	}
	if candidate == "" || len(candidate) > 128 || b.store == nil {
		return "", subject
	}
	machine, err := b.store.GetMachine(candidate)
	if err != nil {
		return "", subject
	}
	return machine.MachineID, machine.DisplayName
}

func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}
