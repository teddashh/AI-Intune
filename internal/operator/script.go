package operator

import "github.com/teddashh/AI-Intune/internal/store"

func (s *Service) ScriptRun(req store.ScriptRunRequest, apply bool, key string, actor Actor) (store.ScriptRunResult, error) {
	return s.store.ScriptRun(req, apply, key, auditFromActor(actor))
}
