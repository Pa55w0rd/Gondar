package server

import "strings"

func (s *Server) searchAPIKeyConfigured() bool {
	key, exists, err := s.m.pg.GetSetting(settingDeepSeekSearchKey)
	return err == nil && exists && strings.TrimSpace(key) != ""
}
